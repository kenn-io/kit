package sqlitevec

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kit/vector"
)

// PendingForGeneration scans the caller's documents table for rows whose
// stamp does not yet match gen, ordered by primary key for stable paging.
// When the schema names a revision column, each row's revision is returned
// so SaveVectors can stamp optimistically.
func (s *Store[K, G]) PendingForGeneration(ctx context.Context, gen G, limit int) ([]vector.Pending[K], error) {
	ordinal, _, err := s.lookupGeneration(ctx, gen)
	if err != nil {
		return nil, err
	}
	columns := fmt.Sprintf("d.%s, d.%s", s.schema.IDColumn, s.schema.ContentColumn)
	args := []any{ordinal, limit}
	if s.schema.RevisionColumn != "" {
		columns = fmt.Sprintf("d.%s, d.%s, d.%s", s.schema.IDColumn, s.schema.ContentColumn, s.schema.RevisionColumn)
	}
	rows, err := s.db.QueryContext(ctx, s.pendingQuery(columns), args...)
	if err != nil {
		return nil, fmt.Errorf("scan pending: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var pending []vector.Pending[K]
	for rows.Next() {
		var p vector.Pending[K]
		var content sql.NullString
		dest := []any{&p.Doc, &content}
		if s.schema.RevisionColumn != "" {
			dest = append(dest, &p.Revision)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan pending row: %w", err)
		}
		p.Content = content.String
		pending = append(pending, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan pending rows: %w", err)
	}
	return pending, nil
}

func (s *Store[K, G]) pendingQuery(columns string) string {
	if s.schema.RevisionColumn != "" {
		return fmt.Sprintf(`
SELECT %s
  FROM %s d
  LEFT JOIN %s stamp ON stamp.ordinal = ? AND stamp.doc_key = d.%s
 WHERE d.%s IS NULL
    OR stamp.doc_key IS NULL
    OR NOT (d.%s IS stamp.revision)
 ORDER BY d.%s LIMIT ?`,
			columns, s.schema.DocsTable, s.stampsTable(), s.schema.IDColumn,
			s.schema.EmbedGenColumn, s.schema.RevisionColumn, s.schema.IDColumn)
	}
	return fmt.Sprintf(
		`SELECT %s
  FROM %s d
  LEFT JOIN %s stamp ON stamp.ordinal = ? AND stamp.doc_key = d.%s
 WHERE d.%s IS NULL
    OR stamp.doc_key IS NULL
 ORDER BY d.%s LIMIT ?`,
		columns, s.schema.DocsTable, s.stampsTable(), s.schema.IDColumn,
		s.schema.EmbedGenColumn, s.schema.IDColumn)
}

// SaveVectors replaces doc's chunk vectors for gen and stamps the document
// as embedded for gen, all in one transaction. When the schema names a
// revision column, the stamp is conditional on revision still matching the
// document's current value; on a mismatch nothing is persisted and the
// returned error wraps vector.ErrStale.
func (s *Store[K, G]) SaveVectors(ctx context.Context, gen G, doc K, revision any, vectors []vector.ChunkVector) error {
	if revision != nil && s.schema.RevisionColumn == "" {
		return fmt.Errorf("revision given for document %v but schema names no revision column", doc)
	}
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

	invalidated, err := s.docInvalidated(ctx, tx, doc)
	if err != nil {
		return err
	}
	if invalidated {
		if err := s.deleteDocumentVectors(ctx, tx, doc); err != nil {
			return err
		}
	} else if err := s.deleteGenerationVectors(ctx, tx, ordinal, doc); err != nil {
		return err
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

	var res sql.Result
	if s.schema.RevisionColumn != "" {
		// IS rather than = so a NULL revision still matches NULL.
		res, err = tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE %s SET %s = ? WHERE %s = ? AND %s IS ?`,
				s.schema.DocsTable, s.schema.EmbedGenColumn, s.schema.IDColumn, s.schema.RevisionColumn),
			gen, doc, revision)
	} else {
		res, err = tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE %s SET %s = ? WHERE %s = ?`, s.schema.DocsTable, s.schema.EmbedGenColumn, s.schema.IDColumn),
			gen, doc)
	}
	if err != nil {
		return fmt.Errorf("stamp embed generation: %w", err)
	}
	stamped, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("stamp embed generation rows: %w", err)
	}
	if stamped == 0 {
		if s.schema.RevisionColumn != "" {
			exists, err := s.docExists(ctx, tx, doc)
			if err != nil {
				return err
			}
			if exists {
				// The document changed between scan and save. Roll back so
				// the stale vectors are not stamped over the newer content;
				// the document stays pending and is re-read next run.
				return fmt.Errorf("document %v: %w", doc, vector.ErrStale)
			}
		}
		// The source row vanished between scan and save (or the key is
		// wrong). Roll back rather than commit vectors with no document,
		// which QueryGeneration would otherwise surface as orphan hits.
		return fmt.Errorf("document %v not present in %s; vectors not persisted", doc, s.schema.DocsTable)
	}
	stampRevision := revision
	if s.schema.RevisionColumn != "" {
		if err := tx.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT %s FROM %s WHERE %s = ?`, s.schema.RevisionColumn, s.schema.DocsTable, s.schema.IDColumn),
			doc).Scan(&stampRevision); err != nil {
			return fmt.Errorf("read stamped revision: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE %s SET revision = ? WHERE doc_key = ? AND revision IS ?`, s.stampsTable()),
			stampRevision, doc, revision); err != nil {
			return fmt.Errorf("advance revision stamps: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (ordinal, doc_key, revision) VALUES (?, ?, ?)
ON CONFLICT(ordinal, doc_key) DO UPDATE SET revision = excluded.revision`, s.stampsTable()),
		ordinal, doc, stampRevision); err != nil {
		return fmt.Errorf("stamp revision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit save vectors: %w", err)
	}
	return nil
}

// DeleteVectors removes doc's vectors and chunk mappings from every
// generation. Callers invoke it when deleting a source document; without
// it the orphaned vectors keep occupying KNN result slots even though
// QueryGeneration filters them out of hits.
func (s *Store[K, G]) DeleteVectors(ctx context.Context, doc K) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete vectors: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.deleteDocumentVectors(ctx, tx, doc); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete vectors: %w", err)
	}
	return nil
}

func (s *Store[K, G]) docInvalidated(ctx context.Context, tx *sql.Tx, doc K) (bool, error) {
	var embedGen any
	err := tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s FROM %s WHERE %s = ?`, s.schema.EmbedGenColumn, s.schema.DocsTable, s.schema.IDColumn), doc).Scan(&embedGen)
	if err == sql.ErrNoRows {
		return false, fmt.Errorf("document %v not present in %s; vectors not persisted", doc, s.schema.DocsTable)
	}
	if err != nil {
		return false, fmt.Errorf("read embed generation: %w", err)
	}
	return embedGen == nil, nil
}

func (s *Store[K, G]) deleteGenerationVectors(ctx context.Context, tx *sql.Tx, ordinal int64, doc K) error {
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
	return nil
}

func (s *Store[K, G]) deleteDocumentVectors(ctx context.Context, tx *sql.Tx, doc K) error {
	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf(`SELECT ordinal, vec_rowid FROM %s WHERE doc_key = ?`, s.chunksTable()), doc)
	if err != nil {
		return fmt.Errorf("read chunk map: %w", err)
	}
	type chunkRef struct{ ordinal, rowid int64 }
	var refs []chunkRef
	for rows.Next() {
		var ref chunkRef
		if err := rows.Scan(&ref.ordinal, &ref.rowid); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan chunk rowid: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read chunk map: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("read chunk map: %w", err)
	}

	for _, ref := range refs {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE rowid = ?`, s.vecTable(ref.ordinal)), ref.rowid); err != nil {
			return fmt.Errorf("delete vector: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE doc_key = ?`, s.chunksTable()), doc); err != nil {
		return fmt.Errorf("delete chunk map: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE doc_key = ?`, s.stampsTable()), doc); err != nil {
		return fmt.Errorf("delete revision stamps: %w", err)
	}
	return nil
}

func (s *Store[K, G]) docExists(ctx context.Context, tx *sql.Tx, doc K) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT 1 FROM %s WHERE %s = ?`, s.schema.DocsTable, s.schema.IDColumn), doc).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check document %v: %w", doc, err)
	}
	return true, nil
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
//
// Hits are joined back to the documents table, so vectors whose source
// row has been deleted are never returned. The join checks existence
// only — never the generation stamp, which records just the newest
// generation and would wrongly hide the active generation's valid
// vectors mid-migration. Orphaned vectors still occupy KNN slots inside
// limit until DeleteVectors removes them.
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
	// joins to the chunk map to recover document keys and to the source
	// table to drop vectors for deleted documents.
	sqlText := fmt.Sprintf(`
WITH knn AS (
    SELECT rowid, distance FROM %s WHERE embedding MATCH %s ORDER BY distance LIMIT ?
)
SELECT c.doc_key, c.chunk_index, knn.distance
  FROM knn
  JOIN %s c ON c.ordinal = ? AND c.vec_rowid = knn.rowid
  JOIN %s d ON d.%s = c.doc_key
 ORDER BY knn.distance`, s.vecTable(ordinal), expr, s.chunksTable(), s.schema.DocsTable, s.schema.IDColumn)
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
