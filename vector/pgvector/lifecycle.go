package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kit/vector"
)

type generationRow struct {
	id          int64
	fingerprint string
	dim         int
	state       vector.State
}

// lookup reads a generation row, locking it when lock is set. It wraps ErrGenerationNotFound when none exists.
func (s *Store[K, G]) lookup(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, gen G, lock bool) (generationRow, error) {
	var r generationRow
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	err := q.QueryRowContext(ctx, fmt.Sprintf(`SELECT id,fingerprint,dimension,state FROM %s WHERE gen_key=$1%s`, s.generations(), suffix), gen).Scan(&r.id, &r.fingerprint, &r.dim, &r.state)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("generation %v: %w", gen, vector.ErrGenerationNotFound)
	}
	if err == nil && !validState(r.state) {
		err = fmt.Errorf("generation %v has unknown state %q", gen, r.state)
	}
	return r, err
}

func validState(state vector.State) bool {
	switch state {
	case vector.StatePending, vector.StateBuilding, vector.StateActive, vector.StateRetired:
		return true
	}
	return false
}

// insertGeneration adds a missing generation. A concurrent insert is read back instead.
func (s *Store[K, G]) insertGeneration(ctx context.Context, tx *sql.Tx, gen G, model vector.Generation, state vector.State) (generationRow, error) {
	r := generationRow{fingerprint: model.Fingerprint(), dim: model.Dimensions, state: state}
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`INSERT INTO %s (gen_key,fingerprint,dimension,state) VALUES ($1,$2,$3,$4) ON CONFLICT(gen_key) DO NOTHING RETURNING id`, s.generations()), gen, r.fingerprint, r.dim, state).Scan(&r.id)
	if errors.Is(err, sql.ErrNoRows) {
		return s.lookup(ctx, tx, gen, true)
	}
	return r, err
}

// reuseGeneration checks an existing row against the request and records the requested state.
// done is true when nothing else is needed.
func (s *Store[K, G]) reuseGeneration(ctx context.Context, tx *sql.Tx, gen G, r generationRow, model vector.Generation, state vector.State) (done bool, err error) {
	if r.state == vector.StateRetired {
		if state == r.state {
			return true, nil
		}
		return false, vector.ErrRetired
	}
	if r.fingerprint != model.Fingerprint() || r.dim != model.Dimensions {
		return false, fmt.Errorf("generation %v has different model", gen)
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET state=$1 WHERE id=$2`, s.generations()), state, r.id)
	return false, err
}

func (s *Store[K, G]) storageExists(ctx context.Context, tx *sql.Tx, id int64) (exists bool, err error) {
	err = tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, s.chunks(id)).Scan(&exists)
	return exists, err
}

// ensureStorage creates a generation's chunk table and HNSW index unless the table exists.
func (s *Store[K, G]) ensureStorage(ctx context.Context, tx *sql.Tx, id int64, dimensions int) error {
	if exists, err := s.storageExists(ctx, tx, id); err != nil || exists {
		return err
	}
	var keyType string
	if err := tx.QueryRowContext(ctx, `SELECT format_type(a.atttypid,a.atttypmod) FROM pg_attribute a WHERE a.attrelid=to_regclass($1) AND a.attname='doc_key' AND a.attnum>0`, s.stamps()).Scan(&keyType); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (doc_key %s NOT NULL REFERENCES %s(%s) ON DELETE CASCADE, chunk_index INTEGER NOT NULL, embedding %s.halfvec(%d) NOT NULL, PRIMARY KEY(doc_key,chunk_index)); CREATE INDEX IF NOT EXISTS %s ON %s USING hnsw (embedding %s.halfvec_cosine_ops)`, s.chunks(id), keyType, quote(s.schema.DocsTable), quote(s.schema.IDColumn), quote(s.ext), dimensions, quote(fmt.Sprintf("%s_chunks_g%d_hnsw", s.schema.Prefix, id)), s.chunks(id), quote(s.ext)))
	if err != nil {
		return fmt.Errorf("create generation storage: %w", err)
	}
	return nil
}

// EnsureGeneration registers a model and creates its chunk table and HNSW index.
// An existing generation with the same model needs no sequence value and no DDL.
func (s *Store[K, G]) EnsureGeneration(ctx context.Context, gen G, model vector.Generation, state vector.State) error {
	if !validState(state) {
		return fmt.Errorf("invalid generation state %q", state)
	}
	if model.Dimensions <= 0 || model.Dimensions > 4000 {
		return fmt.Errorf("invalid generation dimension %d", model.Dimensions)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := s.lookup(ctx, tx, gen, true)
	if errors.Is(err, vector.ErrGenerationNotFound) {
		row, err = s.insertGeneration(ctx, tx, gen, model, state)
	}
	if err != nil {
		return err
	}
	done, err := s.reuseGeneration(ctx, tx, gen, row, model, state)
	if err != nil || done {
		return err
	}
	if err = s.ensureStorage(ctx, tx, row.id, model.Dimensions); err != nil {
		return err
	}
	return tx.Commit()
}

// SetGenerationState records a state without publishing coverage.
func (s *Store[K, G]) SetGenerationState(ctx context.Context, gen G, state vector.State) error {
	if !validState(state) {
		return fmt.Errorf("invalid generation state %q", state)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := s.lookup(ctx, tx, gen, true)
	if err != nil {
		return err
	}
	if row.state == vector.StateRetired && state != row.state {
		return vector.ErrRetired
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET state=$1 WHERE id=$2`, s.generations()), state, row.id); err != nil {
		return err
	}
	return tx.Commit()
}

// Activate publishes a fully covered generation and retires the others.
func (s *Store[K, G]) Activate(ctx context.Context, gen G) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := s.lookup(ctx, tx, gen, true)
	if err != nil {
		return err
	}
	if row.state == vector.StateRetired {
		return vector.ErrRetired
	}
	id := row.id
	var backlog int64
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %s d LEFT JOIN %s stamp ON stamp.generation_id=$1 AND stamp.doc_key=d.%s WHERE NOT %s`, quote(s.schema.DocsTable), s.stamps(), quote(s.schema.IDColumn), s.covered("d", "stamp")), id).Scan(&backlog)
	if err != nil {
		return err
	}
	if backlog > 0 {
		return &vector.UncoveredError{Backlog: backlog}
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET state=CASE WHEN id=$1 THEN $2 ELSE $3 END WHERE id=$1 OR state IN ($2,$4)`, s.generations()), id, vector.StateActive, vector.StateRetired, vector.StateBuilding)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Reclaim removes the storage of a retired generation.
func (s *Store[K, G]) Reclaim(ctx context.Context, gen G) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := s.lookup(ctx, tx, gen, true)
	if err != nil {
		return err
	}
	id := row.id
	if row.state != vector.StateRetired {
		return fmt.Errorf("generation %v is not retired", gen)
	}
	if _, err = tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+s.chunks(id)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE generation_id=$1`, s.stamps()), id); err != nil {
		return err
	}
	return tx.Commit()
}

// ActiveGeneration returns the newest active generation, if one exists.
func (s *Store[K, G]) ActiveGeneration(ctx context.Context) (vector.GenerationInfo[G], bool, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT gen_key, fingerprint, dimension, state FROM %s ORDER BY id DESC`, s.generations()))
	if err != nil {
		return vector.GenerationInfo[G]{}, false, fmt.Errorf("read generations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var active vector.GenerationInfo[G]
	var found bool
	for rows.Next() {
		var info vector.GenerationInfo[G]
		if err := rows.Scan(&info.Key, &info.Fingerprint, &info.Dimension, &info.State); err != nil {
			return active, false, fmt.Errorf("scan generation: %w", err)
		}
		if !validState(info.State) {
			return active, false, fmt.Errorf("generation %v has unknown state %q", info.Key, info.State)
		}
		if !found && info.State == vector.StateActive {
			active, found = info, true
		}
	}
	if err := rows.Err(); err != nil {
		return active, false, fmt.Errorf("read generations: %w", err)
	}
	return active, found, nil
}
