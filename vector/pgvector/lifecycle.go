package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kit/search/postgres"
	"go.kenn.io/kit/vector"
)

type generationRow struct {
	id          int64
	fingerprint string
	dim         int
	state       vector.State
}

// lookup reads a generation row, locking it when lock is set. It rejects persisted states this version cannot interpret.
func (s *Store[K, G]) lookup(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, gen G, lock bool,
) (generationRow, error) {
	var r generationRow
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	err := q.QueryRowContext(ctx, fmt.Sprintf(`SELECT id,fingerprint,dimension,state FROM %s WHERE gen_key=$1%s`, s.generations(), suffix), gen).Scan(&r.id, &r.fingerprint, &r.dim, &r.state)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("generation %v: %w", gen, vector.ErrGenerationNotFound)
	}
	if err == nil && !knownState(r.state) {
		return r, fmt.Errorf("generation %v has unknown generation state %q", gen, r.state)
	}
	return r, err
}

func knownState(state vector.State) bool {
	switch state {
	case vector.StatePending, vector.StateBuilding, vector.StateActive, vector.StateRetired:
		return true
	default:
		return false
	}
}

// lockLifecycle serializes changes to the generation catalog.
func (s *Store[K, G]) lockLifecycle(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('kit/pgvector/lifecycle/' || to_regclass($1)::oid::text, 0))`, s.generations())
	return err
}

// lockGeneration coordinates a generation's saves and lifecycle changes.
func (s *Store[K, G]) lockGeneration(ctx context.Context, tx *sql.Tx, gen G, shared bool) error {
	lock := "pg_advisory_xact_lock"
	if shared {
		lock += "_shared"
	}
	_, err := tx.ExecContext(ctx, "SELECT "+lock+"(hashtextextended('kit/pgvector/generation/' || to_regclass($1)::oid::text || '/' || $2::text, 0))", s.generations(), gen)
	return err
}

// insertGeneration adds a missing generation. A concurrent insert is read back instead.
func (s *Store[K, G]) insertGeneration(ctx context.Context, tx *sql.Tx, gen G, fingerprint string, dimensions int, state vector.State) (generationRow, error) {
	r := generationRow{fingerprint: fingerprint, dim: dimensions, state: state}
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`INSERT INTO %s (gen_key,fingerprint,dimension,state) VALUES ($1,$2,$3,$4) ON CONFLICT(gen_key) DO NOTHING RETURNING id`, s.generations()), gen, fingerprint, dimensions, state).Scan(&r.id)
	if errors.Is(err, sql.ErrNoRows) {
		return s.lookup(ctx, tx, gen, true)
	}
	return r, err
}

// ensureStorage creates a generation's chunk table and HNSW index unless the table exists.
func (s *Store[K, G]) ensureStorage(ctx context.Context, tx *sql.Tx, id int64, dimensions int) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, s.chunks(id)).Scan(&exists); err != nil || exists {
		return err
	}
	var keyType string
	if err := tx.QueryRowContext(ctx, `SELECT format_type(a.atttypid,a.atttypmod) FROM pg_attribute a WHERE a.attrelid=to_regclass($1) AND a.attname='doc_key' AND a.attnum>0`, s.stamps()).Scan(&keyType); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (doc_key %s NOT NULL REFERENCES %s(%s) ON DELETE CASCADE, chunk_index INTEGER NOT NULL, embedding %s.halfvec(%d) NOT NULL, PRIMARY KEY(doc_key,chunk_index)); CREATE INDEX IF NOT EXISTS %s ON %s USING hnsw (embedding %s.halfvec_cosine_ops)`, s.chunks(id), keyType, postgres.QuoteIdentifier(s.schema.DocsTable), postgres.QuoteIdentifier(s.schema.IDColumn), postgres.QuoteIdentifier(s.ext), dimensions, postgres.QuoteIdentifier(fmt.Sprintf("%s_chunks_g%d_hnsw", s.schema.Prefix, id)), s.chunks(id), postgres.QuoteIdentifier(s.ext)))
	if err != nil {
		return fmt.Errorf("create generation storage: %w", err)
	}
	return nil
}

// EnsureGeneration registers a model. Default storage creates a chunk table and HNSW index.
// An existing generation with the same model needs no sequence value and no DDL.
// The state must be pending, building, or active. Shared storage permits
// re-ensuring a retired generation as building.
func (s *Store[K, G]) EnsureGeneration(ctx context.Context, gen G, model vector.Generation, state vector.State) error {
	if state != vector.StatePending && state != vector.StateBuilding && state != vector.StateActive {
		return fmt.Errorf("invalid generation state %q", state)
	}
	if model.Dimensions <= 0 || model.Dimensions > 4000 {
		return fmt.Errorf("invalid generation dimension %d", model.Dimensions)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockLifecycle(ctx, tx); err != nil {
		return err
	}
	if err := s.lockGeneration(ctx, tx, gen, false); err != nil {
		return err
	}
	row, err := s.lookup(ctx, tx, gen, true)
	if errors.Is(err, vector.ErrGenerationNotFound) {
		row, err = s.insertGeneration(ctx, tx, gen, model.Fingerprint(), model.Dimensions, state)
	}
	if err != nil {
		return err
	}
	if row.state == vector.StateRetired && (!s.schema.SharedChunks || state != vector.StateBuilding) {
		return vector.ErrRetired
	}
	if row.fingerprint != model.Fingerprint() || row.dim != model.Dimensions {
		return fmt.Errorf("generation %v has different model", gen)
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET state=$1 WHERE id=$2`, s.generations()), state, row.id); err != nil {
		return err
	}
	if !s.schema.SharedChunks {
		if err = s.ensureStorage(ctx, tx, row.id, model.Dimensions); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetGenerationState records a lifecycle decision. A retired generation cannot
// leave retired because Reclaim may have dropped its storage. Activate checks
// coverage before publishing a generation.
func (s *Store[K, G]) SetGenerationState(ctx context.Context, gen G, state vector.State) error {
	if !knownState(state) {
		return fmt.Errorf("invalid generation state %q", state)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockLifecycle(ctx, tx); err != nil {
		return err
	}
	if err := s.lockGeneration(ctx, tx, gen, false); err != nil {
		return err
	}
	row, err := s.lookup(ctx, tx, gen, true)
	if err != nil {
		return err
	}
	if row.state == vector.StateRetired && state != vector.StateRetired {
		return fmt.Errorf("generation %v: %w", gen, vector.ErrRetired)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET state=$1 WHERE id=$2`, s.generations()), state, row.id); err != nil {
		return err
	}
	return tx.Commit()
}

// Activate publishes a fully covered generation and retires the others. Retired
// generations keep their storage.
func (s *Store[K, G]) Activate(ctx context.Context, gen G) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockLifecycle(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT gen_key FROM %s WHERE gen_key=$1 OR state IN ($2,$3) ORDER BY gen_key`, s.generations()), gen, vector.StateActive, vector.StateBuilding)
	if err != nil {
		return err
	}
	var gens []G
	for rows.Next() {
		var key G
		if err := rows.Scan(&key); err != nil {
			_ = rows.Close()
			return err
		}
		gens = append(gens, key)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, key := range gens {
		if err := s.lockGeneration(ctx, tx, key, false); err != nil {
			return err
		}
	}
	row, err := s.lookup(ctx, tx, gen, true)
	if err != nil {
		return err
	}
	if row.state == vector.StateRetired {
		return vector.ErrRetired
	}
	var backlog int64
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %s d LEFT JOIN %s stamp ON stamp.generation_id=$1 AND stamp.doc_key=d.%s WHERE NOT %s`, postgres.QuoteIdentifier(s.schema.DocsTable), s.stamps(), postgres.QuoteIdentifier(s.schema.IDColumn), s.covered("d", "stamp")), row.id).Scan(&backlog)
	if err != nil {
		return err
	}
	if backlog > 0 {
		return &vector.UncoveredError{Backlog: backlog}
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET state=CASE WHEN id=$1 THEN $2 ELSE $3 END WHERE id=$1 OR state IN ($2,$4)`, s.generations()), row.id, vector.StateActive, vector.StateRetired, vector.StateBuilding)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Reclaim removes a retired generation's chunks and stamps while keeping
// its generation row. Default storage requires a role that owns the tables.
func (s *Store[K, G]) Reclaim(ctx context.Context, gen G) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return fmt.Errorf("begin generation reclamation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockLifecycle(ctx, tx); err != nil {
		return err
	}
	if err := s.lockGeneration(ctx, tx, gen, false); err != nil {
		return err
	}
	row, err := s.lookup(ctx, tx, gen, true)
	if err != nil {
		return err
	}
	if row.state != vector.StateRetired {
		return fmt.Errorf("generation %v is %s; reclaim only a retired generation", gen, row.state)
	}
	q := "DROP TABLE IF EXISTS " + s.chunks(row.id)
	args := []any{}
	if s.schema.SharedChunks {
		q = "DELETE FROM " + s.chunkTable(row.id) + " WHERE generation_id=$1"
		args = append(args, row.id)
	}
	if _, err = tx.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("drop retired vectors for generation %v: %w", gen, err)
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE generation_id=$1`, s.stamps()), row.id); err != nil {
		return fmt.Errorf("delete retired stamps for generation %v: %w", gen, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit generation reclamation: %w", err)
	}
	return nil
}
