package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	"go.kenn.io/kit/search/postgres"
	"go.kenn.io/kit/search/sqlquery"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/internal/vectorliteral"
)

// PendingForGeneration returns uncovered documents in key order.
func (s *Store[K, G]) PendingForGeneration(ctx context.Context, gen G, after *K, limit int) ([]vector.Pending[K], error) {
	row, err := s.lookup(ctx, s.db, gen, false)
	if err != nil {
		return nil, err
	}
	if row.state == vector.StateRetired {
		return nil, vector.ErrRetired
	}
	args := []any{row.id}
	cursor := ""
	if after != nil {
		args = append(args, *after)
		cursor = " AND d." + postgres.QuoteIdentifier(s.schema.IDColumn) + " > $2"
	}
	args = append(args, limit)
	q := fmt.Sprintf(`SELECT d.%s,d.%s,d.%s FROM %s d LEFT JOIN %s stamp ON stamp.generation_id=$1 AND stamp.doc_key=d.%s WHERE NOT %s%s ORDER BY d.%s LIMIT $%d`, postgres.QuoteIdentifier(s.schema.IDColumn), postgres.QuoteIdentifier(s.schema.ContentColumn), postgres.QuoteIdentifier(s.schema.RevisionColumn), postgres.QuoteIdentifier(s.schema.DocsTable), s.stamps(), postgres.QuoteIdentifier(s.schema.IDColumn), s.covered("d", "stamp"), cursor, postgres.QuoteIdentifier(s.schema.IDColumn), len(args))
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
	for i, component := range v {
		value := float64(component)
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return "", fmt.Errorf("pgvector: vector component %d is non-finite", i)
		}
		maxComponent = max(maxComponent, math.Abs(value))
	}
	if maxComponent == 0 {
		return "", errors.New("pgvector: vector has zero norm")
	}
	scaled := make([]float32, len(v))
	for i, component := range v {
		scaled[i] = float32(float64(component) / maxComponent)
	}
	return vectorliteral.Format(scaled), nil
}

// SaveVectors atomically checks the document revision, replaces chunks, and stamps it.
func (s *Store[K, G]) SaveVectors(ctx context.Context, gen G, doc K, revision any, vectors []vector.ChunkVector) error {
	tx, err := s.beginWithOptions(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.lockLifecycle(ctx, tx, true); err != nil {
		return err
	}
	row, err := s.lookup(ctx, tx, gen, false)
	if err != nil {
		return err
	}
	if row.state == vector.StateRetired {
		return vector.ErrRetired
	}
	if err = s.saveDocument(ctx, tx, row, doc, revision, vectors); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store[K, G]) saveDocument(ctx context.Context, tx *sql.Tx, row generationRow, doc K, revision any, vectors []vector.ChunkVector) error {
	var err error
	id := row.id
	halfvecs := make([]string, len(vectors))
	indexes := make([]int64, len(vectors))
	for i, cv := range vectors {
		if len(cv.Vector) != row.dim {
			return fmt.Errorf("document %v chunk %d has %d dimensions, expected %d", doc, cv.ChunkIndex, len(cv.Vector), row.dim)
		}
		halfvecs[i], err = halfvec(cv.Vector)
		if err != nil {
			return fmt.Errorf("document %v chunk %d: %w", doc, cv.ChunkIndex, err)
		}
		indexes[i] = int64(cv.ChunkIndex)
	}
	var matches bool
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s IS NOT DISTINCT FROM $2 FROM %s WHERE %s=$1 FOR NO KEY UPDATE`, postgres.QuoteIdentifier(s.schema.RevisionColumn), postgres.QuoteIdentifier(s.schema.DocsTable), postgres.QuoteIdentifier(s.schema.IDColumn)), doc, revision).Scan(&matches)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("document %v missing: %w", doc, vector.ErrStale)
	}
	if err != nil {
		return err
	}
	if !matches {
		return vector.ErrStale
	}
	deleteQuery := fmt.Sprintf(`DELETE FROM %s WHERE doc_key=$1`, s.chunkTable(id))
	deleteArgs := []any{doc}
	if s.schema.SharedChunks {
		deleteQuery += " AND generation_id=$2"
		deleteArgs = append(deleteArgs, id)
	}
	if _, err = tx.ExecContext(ctx, deleteQuery, deleteArgs...); err != nil {
		return err
	}
	if len(vectors) > 0 {
		cols, vals, args := "doc_key", "$1", []any{doc, indexes, halfvecs}
		if s.schema.SharedChunks {
			cols, vals, args = "generation_id,doc_key", "$4,$1", append(args, id)
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (%s,chunk_index,embedding) SELECT %s,chunk_index,embedding::%s.halfvec FROM unnest($2::bigint[],$3::text[]) AS chunks(chunk_index,embedding)`, s.chunkTable(id), cols, vals, postgres.QuoteIdentifier(s.ext)), args...); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (generation_id,doc_key,revision) SELECT $1,%s,%s FROM %s WHERE %s=$2 ON CONFLICT (generation_id,doc_key) DO UPDATE SET revision=excluded.revision`, s.stamps(), postgres.QuoteIdentifier(s.schema.IDColumn), postgres.QuoteIdentifier(s.schema.RevisionColumn), postgres.QuoteIdentifier(s.schema.DocsTable), postgres.QuoteIdentifier(s.schema.IDColumn)), id, doc)
	if err != nil {
		return err
	}
	return nil
}

// LiveGenerations returns building generations before active generations, newest first.
func (s *Store[K, G]) LiveGenerations(ctx context.Context) ([]G, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT gen_key,state FROM %s WHERE state IN ($1,$2) OR state NOT IN ($3,$1,$2,$4) ORDER BY CASE WHEN state=$1 THEN 0 WHEN state=$2 THEN 1 ELSE 2 END,id DESC`, s.generations()), vector.StateBuilding, vector.StateActive, vector.StatePending, vector.StateRetired)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var gens []G
	for rows.Next() {
		var gen G
		var state vector.State
		if err := rows.Scan(&gen, &state); err != nil {
			return nil, err
		}
		if !knownState(state) {
			return nil, fmt.Errorf("generation %v has unknown generation state %q", gen, state)
		}
		if state == vector.StateBuilding || state == vector.StateActive {
			gens = append(gens, gen)
		}
	}
	return gens, rows.Err()
}

// QueryGeneration returns current chunk hits ordered by cosine similarity. Stale
// chunks are filtered while the index is scanned, so they do not take result slots.
func (s *Store[K, G]) QueryGeneration(ctx context.Context, gen G, query vector.Vector, limit int) ([]vector.Hit[K], error) {
	return s.QueryGenerationWithPredicate(ctx, gen, query, limit, sqlquery.Predicate{})
}

// QueryGenerationWithPredicate applies a trusted source filter before the
// candidate limit. The predicate refers to the documents table as d and uses
// anonymous ? placeholders.
func (s *Store[K, G]) QueryGenerationWithPredicate(ctx context.Context, gen G, query vector.Vector, limit int, predicate sqlquery.Predicate) ([]vector.Hit[K], error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := s.lookup(ctx, tx, gen, false)
	if err != nil {
		return nil, err
	}
	if len(query) != row.dim {
		return nil, fmt.Errorf("query has %d dimensions, expected %d", len(query), row.dim)
	}
	sqlText, args, err := s.candidateQuery(row.id, query, limit, predicate)
	if err != nil {
		return nil, err
	}
	if !s.schema.SharedChunks {
		reclaimed, err := s.lockChunks(ctx, tx, gen, row)
		if err != nil {
			return nil, fmt.Errorf("lock generation storage: %w", err)
		}
		if reclaimed {
			return nil, nil
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL hnsw.ef_search = %d`, min(max(limit, 40), 1000))); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `SET LOCAL hnsw.iterative_scan = strict_order`); err != nil {
			return nil, err
		}
	}
	rows, err := tx.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	hits, err := scanHits[K](rows)
	if err != nil {
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

func (s *Store[K, G]) lockChunks(ctx context.Context, tx *sql.Tx, gen G, row generationRow) (bool, error) {
	_, err := tx.ExecContext(ctx, "LOCK TABLE "+s.chunks(row.id)+" IN ACCESS SHARE MODE")
	if err == nil {
		return false, nil
	}
	if ctx.Err() != nil {
		return false, errors.Join(err, ctx.Err())
	}
	if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
		return false, errors.Join(err, fmt.Errorf("rollback failed query transaction: %w", rollbackErr))
	}
	current, lookupErr := s.lookup(ctx, s.db, gen, false)
	if lookupErr != nil {
		return false, errors.Join(err, lookupErr)
	}
	if current.state != vector.StateRetired {
		return false, err
	}
	var exists bool
	if checkErr := s.db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, s.chunks(current.id)).Scan(&exists); checkErr != nil {
		return false, errors.Join(err, fmt.Errorf("check retired generation storage: %w", checkErr))
	}
	if !exists {
		return true, nil
	}
	return false, err
}

// candidateQuery selects the nearest fresh chunks and returns their stamp revision.
// The inner ORDER BY is distance alone; ties are broken by document key after
// the candidate limit. Default storage uses an HNSW index and iterative scans
// to fill the limit while checking freshness. Shared storage searches exactly
// when the caller has not installed a vector index.
func (s *Store[K, G]) candidateQuery(id int64, query vector.Vector, limit int, predicate sqlquery.Predicate) (string, []any, error) {
	ext, docs, key := postgres.QuoteIdentifier(s.ext), postgres.QuoteIdentifier(s.schema.DocsTable), postgres.QuoteIdentifier(s.schema.IDColumn)
	queryVector, err := halfvec(query)
	if err != nil {
		return "", nil, fmt.Errorf("query vector: %w", err)
	}
	args := []any{queryVector, id}
	predicateSQL := strings.TrimSpace(predicate.SQL)
	if predicateSQL == "" && len(predicate.Args) != 0 {
		return "", nil, errors.New("pgvector: predicate args require SQL")
	}
	if predicateSQL != "" {
		var last int
		predicateSQL, last = postgres.RebasePlaceholders(predicateSQL, len(args))
		if used := last - len(args); used != len(predicate.Args) {
			return "", nil, fmt.Errorf("pgvector: source predicate has %d placeholders, but %d args", used, len(predicate.Args))
		}
		args = append(args, predicate.Args...)
		predicateSQL = " AND (" + predicateSQL + ")"
	}
	limitArg := len(args) + 1
	args = append(args, limit)
	distance := fmt.Sprintf(`c.embedding OPERATOR(%s.<=>) $1::%s.halfvec`, ext, ext)
	filter := ""
	if s.schema.SharedChunks {
		filter = " AND c.generation_id=$2"
	}
	text := fmt.Sprintf(`SELECT candidate.doc_key,candidate.revision,candidate.score,candidate.chunk_index FROM (SELECT c.doc_key,stamp.revision,1-(%s) AS score,c.chunk_index FROM %s c JOIN %s d ON d.%s=c.doc_key JOIN %s stamp ON stamp.generation_id=$2 AND stamp.doc_key=d.%s WHERE stamp.revision IS NOT DISTINCT FROM d.%s%s%s ORDER BY %s LIMIT $%d) candidate ORDER BY candidate.score DESC,candidate.doc_key`,
		distance, s.chunkTable(id), docs, key, s.stamps(), key, postgres.QuoteIdentifier(s.schema.RevisionColumn), filter, predicateSQL, distance, limitArg)
	return text, args, nil
}

func scanHits[K comparable](rows *sql.Rows) ([]vector.Hit[K], error) {
	var hits []vector.Hit[K]
	for rows.Next() {
		var hit vector.Hit[K]
		if err := rows.Scan(&hit.Doc, &hit.Revision, &hit.Score, &hit.ChunkIndex); err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}
