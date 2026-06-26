package sqlitevec

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kit/vector"
)

// PendingForGeneration scans the caller's documents table for rows whose
// stamp does not yet match gen, ordered by primary key for stable paging.
func (s *Store[K, G]) PendingForGeneration(ctx context.Context, gen G, limit int) ([]vector.Pending[K], error) {
	query := fmt.Sprintf(
		`SELECT %s, %s FROM %s WHERE %s IS NULL OR %s <> ? ORDER BY %s LIMIT ?`,
		s.schema.IDColumn, s.schema.ContentColumn, s.schema.DocsTable,
		s.schema.EmbedGenColumn, s.schema.EmbedGenColumn, s.schema.IDColumn)
	rows, err := s.db.QueryContext(ctx, query, gen, limit)
	if err != nil {
		return nil, fmt.Errorf("scan pending: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var pending []vector.Pending[K]
	for rows.Next() {
		var p vector.Pending[K]
		if err := rows.Scan(&p.Doc, &p.Content); err != nil {
			return nil, fmt.Errorf("scan pending row: %w", err)
		}
		pending = append(pending, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan pending rows: %w", err)
	}
	return pending, nil
}

// SaveVectors replaces doc's chunk vectors for gen and stamps the document
// as embedded for gen, all in one transaction.
func (s *Store[K, G]) SaveVectors(ctx context.Context, gen G, doc K, vectors []vector.ChunkVector) error {
	ordinal, dimension, err := s.lookupGeneration(ctx, gen)
	if err != nil {
		return err
	}
	for _, cv := range vectors {
		if len(cv.Vector) != dimension {
			return fmt.Errorf("chunk %d has %d dimensions, generation expects %d", cv.ChunkIndex, len(cv.Vector), dimension)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save vectors: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Drop any prior vectors for this document so re-embedding is clean.
	rowids, err := s.docRowids(ctx, tx, ordinal, doc)
	if err != nil {
		return err
	}
	for _, rowid := range rowids {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE rowid = ?`, s.vecTable(ordinal)), rowid); err != nil {
			return fmt.Errorf("delete stale vector: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE ordinal = ? AND doc_key = ?`, s.chunksTable()), ordinal, doc); err != nil {
		return fmt.Errorf("delete stale chunk map: %w", err)
	}

	for _, cv := range vectors {
		expr, value, err := vectorValue(cv.Vector)
		if err != nil {
			return fmt.Errorf("serialize vector: %w", err)
		}
		res, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (embedding) VALUES (%s)`, s.vecTable(ordinal), expr), value)
		if err != nil {
			return fmt.Errorf("insert vector: %w", err)
		}
		rowid, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("vector rowid: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO %s (ordinal, doc_key, chunk_index, vec_rowid) VALUES (?, ?, ?, ?)`, s.chunksTable()),
			ordinal, doc, cv.ChunkIndex, rowid); err != nil {
			return fmt.Errorf("insert chunk map: %w", err)
		}
	}

	res, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET %s = ? WHERE %s = ?`, s.schema.DocsTable, s.schema.EmbedGenColumn, s.schema.IDColumn),
		gen, doc)
	if err != nil {
		return fmt.Errorf("stamp embed generation: %w", err)
	}
	stamped, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("stamp embed generation rows: %w", err)
	}
	if stamped == 0 {
		// The source row vanished between scan and save (or the key is
		// wrong). Roll back rather than commit vectors with no document,
		// which QueryGeneration would otherwise surface as orphan hits.
		return fmt.Errorf("document %v not present in %s; vectors not persisted", doc, s.schema.DocsTable)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit save vectors: %w", err)
	}
	return nil
}

func (s *Store[K, G]) docRowids(ctx context.Context, tx txQuerier, ordinal int64, doc K) ([]int64, error) {
	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf(`SELECT vec_rowid FROM %s WHERE ordinal = ? AND doc_key = ?`, s.chunksTable()), ordinal, doc)
	if err != nil {
		return nil, fmt.Errorf("read chunk map: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var rowids []int64
	for rows.Next() {
		var rowid int64
		if err := rows.Scan(&rowid); err != nil {
			return nil, fmt.Errorf("scan chunk rowid: %w", err)
		}
		rowids = append(rowids, rowid)
	}
	return rowids, rows.Err()
}

// LiveGenerations returns building and active generations, building first,
// so Merge prefers the newer generation when a document is in both.
func (s *Store[K, G]) LiveGenerations(ctx context.Context) ([]G, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
SELECT gen_key FROM %s
 WHERE state IN (?, ?)
 ORDER BY CASE state WHEN ? THEN 0 ELSE 1 END, ordinal`, s.generationsTable()),
		string(StateBuilding), string(StateActive), string(StateBuilding))
	if err != nil {
		return nil, fmt.Errorf("list live generations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var gens []G
	for rows.Next() {
		var gen G
		if err := rows.Scan(&gen); err != nil {
			return nil, fmt.Errorf("scan generation key: %w", err)
		}
		gens = append(gens, gen)
	}
	return gens, rows.Err()
}

// QueryGeneration runs a cosine KNN search within gen's vec0 table and
// maps each neighbor back to its document and chunk. Score is the cosine
// similarity (1 - cosine distance), so higher is more similar.
func (s *Store[K, G]) QueryGeneration(ctx context.Context, gen G, query vector.Vector, limit int) ([]vector.Hit[K], error) {
	ordinal, dimension, err := s.lookupGeneration(ctx, gen)
	if err != nil {
		return nil, err
	}
	if len(query) != dimension {
		return nil, fmt.Errorf("query has %d dimensions, generation expects %d", len(query), dimension)
	}
	expr, value, err := vectorValue(query)
	if err != nil {
		return nil, fmt.Errorf("serialize query: %w", err)
	}
	// The KNN runs against the vec0 table alone (its required form), then
	// joins to the chunk map to recover document keys.
	sqlText := fmt.Sprintf(`
WITH knn AS (
    SELECT rowid, distance FROM %s WHERE embedding MATCH %s ORDER BY distance LIMIT ?
)
SELECT c.doc_key, c.chunk_index, knn.distance
  FROM knn JOIN %s c ON c.ordinal = ? AND c.vec_rowid = knn.rowid
 ORDER BY knn.distance`, s.vecTable(ordinal), expr, s.chunksTable())
	rows, err := s.db.QueryContext(ctx, sqlText, value, limit, ordinal)
	if err != nil {
		return nil, fmt.Errorf("query generation: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var hits []vector.Hit[K]
	for rows.Next() {
		var (
			doc        K
			chunkIndex int
			distance   float64
		)
		if err := rows.Scan(&doc, &chunkIndex, &distance); err != nil {
			return nil, fmt.Errorf("scan hit: %w", err)
		}
		hits = append(hits, vector.Hit[K]{Doc: doc, ChunkIndex: chunkIndex, Score: float32(1 - distance)})
	}
	return hits, rows.Err()
}

// txQuerier is the read surface shared by *sql.DB and *sql.Tx.
type txQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
