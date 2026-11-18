package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"go.kenn.io/kit/search/postgres"
	"go.kenn.io/kit/search/sqlquery"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/internal/vectorliteral"
)

// PendingForGeneration returns uncovered documents in key order.
func (s *Store[K, G]) PendingForGeneration(ctx context.Context, gen G, after *K, limit int) ([]vector.Pending[K], error) {
	id, _, _, err := s.lookup(ctx, s.db, gen, false)
	if err != nil {
		return nil, err
	}
	args := []any{id}
	cursor := ""
	if after != nil {
		args = append(args, *after)
		cursor = " AND d." + quote(s.schema.IDColumn) + " > $2"
	}
	args = append(args, limit)
	q := fmt.Sprintf(`SELECT d.%s,d.%s,d.%s FROM %s d LEFT JOIN %s stamp ON stamp.generation_id=$1 AND stamp.doc_key=d.%s WHERE NOT %s%s ORDER BY d.%s LIMIT $%d`, quote(s.schema.IDColumn), quote(s.schema.ContentColumn), quote(s.schema.RevisionColumn), quote(s.schema.DocsTable), s.stamps(), quote(s.schema.IDColumn), s.covered("d", "stamp"), cursor, quote(s.schema.IDColumn), len(args))
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []vector.Pending[K]
	for rows.Next() {
		var p vector.Pending[K]
		var content sql.NullString
		if err := rows.Scan(&p.Doc, &content, &p.Revision); err != nil {
			return nil, err
		}
		p.Content = content.String
		out = append(out, p)
	}
	return out, rows.Err()
}

// halfvec preserves cosine direction by scaling the largest component to one.
// Scaling in float64 handles the full range of finite float32 inputs.
func halfvec(v vector.Vector) (string, error) {
	var maxComponent float64
	for _, component := range v {
		value := math.Abs(float64(component))
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return "", errors.New("vector has a non-finite component")
		}
		maxComponent = max(maxComponent, value)
	}
	if maxComponent == 0 {
		return "", errors.New("vector has zero norm")
	}
	scaled := make([]float32, len(v))
	for i, component := range v {
		scaled[i] = float32(float64(component) / maxComponent)
	}
	return vectorliteral.Format(scaled), nil
}

// SaveVectors atomically checks the document revision, replaces chunks, and stamps it.
func (s *Store[K, G]) SaveVectors(ctx context.Context, gen G, doc K, revision any, vectors []vector.ChunkVector) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	id, dim, state, err := s.lookup(ctx, tx, gen, true)
	if err != nil {
		return err
	}
	if state == vector.StateRetired {
		return vector.ErrRetired
	}
	for _, cv := range vectors {
		if len(cv.Vector) != dim {
			return fmt.Errorf("chunk %d has %d dimensions, expected %d", cv.ChunkIndex, len(cv.Vector), dim)
		}
	}
	var matches bool
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s IS NOT DISTINCT FROM $2 FROM %s WHERE %s=$1 FOR UPDATE`, quote(s.schema.RevisionColumn), quote(s.schema.DocsTable), quote(s.schema.IDColumn)), doc, revision).Scan(&matches)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("document %v missing: %w", doc, vector.ErrStale)
	}
	if err != nil {
		return err
	}
	if !matches {
		return vector.ErrStale
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE doc_key=$1`, s.chunks(id)), doc); err != nil {
		return err
	}
	for _, cv := range vectors {
		value, err := halfvec(cv.Vector)
		if err != nil {
			return fmt.Errorf("chunk %d: %w", cv.ChunkIndex, err)
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (doc_key,chunk_index,embedding) VALUES ($1,$2,$3::%s.halfvec)`, s.chunks(id), quote(s.ext)), doc, cv.ChunkIndex, value); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (generation_id,doc_key,revision) SELECT $1,%s,%s FROM %s WHERE %s=$2 ON CONFLICT (generation_id,doc_key) DO UPDATE SET revision=excluded.revision`, s.stamps(), quote(s.schema.IDColumn), quote(s.schema.RevisionColumn), quote(s.schema.DocsTable), quote(s.schema.IDColumn)), id, doc)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// LiveGenerations returns building generations before active generations, newest first.
func (s *Store[K, G]) LiveGenerations(ctx context.Context) ([]G, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT gen_key FROM %s WHERE state IN ($1,$2) ORDER BY CASE WHEN state=$1 THEN 0 ELSE 1 END,id DESC`, s.generations()), vector.StateBuilding, vector.StateActive)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var gens []G
	for rows.Next() {
		var gen G
		if err := rows.Scan(&gen); err != nil {
			return nil, err
		}
		gens = append(gens, gen)
	}
	return gens, rows.Err()
}

// QueryGeneration returns current chunk hits ordered by cosine similarity.
func (s *Store[K, G]) QueryGeneration(ctx context.Context, gen G, query vector.Vector, limit int) ([]vector.Hit[K], error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	id, dim, _, err := s.lookup(ctx, tx, gen, false)
	if err != nil {
		return nil, err
	}
	if len(query) != dim {
		return nil, fmt.Errorf("query has %d dimensions, expected %d", len(query), dim)
	}
	if limit <= 0 {
		return nil, errors.New("limit must be positive")
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL hnsw.ef_search = %d`, min(max(limit, 40), 1000))); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL hnsw.iterative_scan = strict_order`); err != nil {
		return nil, err
	}
	value, err := halfvec(query)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	predicate := fmt.Sprintf(`EXISTS (SELECT 1 FROM %s src JOIN %s stamp ON stamp.generation_id=? AND stamp.doc_key=src.%s WHERE src.%s=d.doc_key AND %s)`, quote(s.schema.DocsTable), s.stamps(), quote(s.schema.IDColumn), quote(s.schema.IDColumn), s.covered("src", "stamp"))
	q, err := postgres.BuildVector(postgres.VectorRequest{Mapping: postgres.VectorMapping{SourceTable: fmt.Sprintf("%s_chunks_g%d", s.schema.Prefix, id), SourceKey: "doc_key", VectorColumn: "embedding"}, Query: value, VectorType: "halfvec", TypeSchema: s.ext, SourcePredicate: sqlquery.Predicate{SQL: predicate, Args: []any{id}}, ExtraSourceCols: []sqlquery.Column{{Name: "chunk_index", As: "chunk_index"}}, CandidateLimit: limit})
	if err != nil {
		return nil, err
	}
	q.SQL = fmt.Sprintf(`SELECT c.doc_key,stamp.revision,c.score,c.chunk_index FROM (%s) c JOIN %s stamp ON stamp.generation_id=$2 AND stamp.doc_key=c.doc_key ORDER BY c.score DESC,c.doc_key`, q.SQL, s.stamps())
	rows, err := tx.QueryContext(ctx, q.SQL, q.Args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var hits []vector.Hit[K]
	for rows.Next() {
		var hit vector.Hit[K]
		if err := rows.Scan(&hit.Doc, &hit.Revision, &hit.Score, &hit.ChunkIndex); err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return hits, nil
}

// DeleteVectors removes a document's vectors and stamps across generations.
func (s *Store[K, G]) DeleteVectors(ctx context.Context, doc K) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT id FROM %s FOR UPDATE`, s.generations()))
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, s.chunks(id)).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE doc_key=$1`, s.chunks(id)), doc); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE doc_key=$1`, s.stamps()), doc); err != nil {
		return err
	}
	return tx.Commit()
}
