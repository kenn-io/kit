package pgvector

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kit/search/postgres"
	"go.kenn.io/kit/vector"
)

// Document holds one document's revision and chunks to save.
type Document[K comparable] struct {
	Doc      K
	Revision any
	Vectors  []vector.ChunkVector
}

// SaveDocuments replaces chunks and stamps for current documents in tx. It
// returns stale or missing keys in input order. The caller owns the transaction.
// Each document key must appear at most once. The transaction must use READ
// COMMITTED isolation. Save and lifecycle locks last until it ends. On error,
// the caller must roll back the transaction.
func (s *Store[K, G]) SaveDocuments(ctx context.Context, tx *sql.Tx, gen G, docs []Document[K]) ([]K, error) {
	if err := s.lockLifecycle(ctx, tx, true); err != nil {
		return nil, err
	}
	row, err := s.lookup(ctx, tx, gen, false)
	if err != nil {
		return nil, err
	}
	if row.state == vector.StateRetired {
		return nil, vector.ErrRetired
	}
	if len(docs) == 0 {
		return nil, nil
	}
	keys := make([]K, len(docs))
	revisions := make([]any, len(docs))
	seen := make(map[K]bool, len(docs))
	encoded := make([][]string, len(docs))
	for i, doc := range docs {
		if seen[doc.Doc] {
			return nil, fmt.Errorf("duplicate document %v", doc.Doc)
		}
		seen[doc.Doc] = true
		keys[i], revisions[i] = doc.Doc, doc.Revision
		for _, chunk := range doc.Vectors {
			if len(chunk.Vector) != row.dim {
				return nil, fmt.Errorf("chunk %d has %d dimensions, expected %d", chunk.ChunkIndex, len(chunk.Vector), row.dim)
			}
			embedding, err := halfvec(chunk.Vector)
			if err != nil {
				return nil, fmt.Errorf("chunk %d: %w", chunk.ChunkIndex, err)
			}
			encoded[i] = append(encoded[i], embedding)
		}
	}
	key, rev, table := postgres.QuoteIdentifier(s.schema.IDColumn), postgres.QuoteIdentifier(s.schema.RevisionColumn), postgres.QuoteIdentifier(s.schema.DocsTable)
	q := fmt.Sprintf(`SELECT input.ordinality FROM %s d JOIN unnest(ARRAY(SELECT %s FROM %s WHERE false) || $1, ARRAY(SELECT %s FROM %s WHERE false) || $2) WITH ORDINALITY AS input(doc_key,revision,ordinality) ON d.%s=input.doc_key WHERE d.%s IS NOT DISTINCT FROM input.revision`, table, key, table, rev, table, key, rev)
	rows, err := tx.QueryContext(ctx, q, keys, revisions)
	if err != nil {
		return nil, err
	}
	current := make([]bool, len(docs))
	for rows.Next() {
		var ordinal int
		if err := rows.Scan(&ordinal); err != nil {
			_ = rows.Close()
			return nil, err
		}
		current[ordinal-1] = true
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	var stale, accepted, chunkKeys []K
	var acceptedRevisions []any
	var indexes []int64
	var embeddings []string
	for i, doc := range docs {
		if !current[i] {
			stale = append(stale, doc.Doc)
			continue
		}
		accepted = append(accepted, doc.Doc)
		acceptedRevisions = append(acceptedRevisions, doc.Revision)
		for j, chunk := range doc.Vectors {
			chunkKeys = append(chunkKeys, doc.Doc)
			indexes = append(indexes, int64(chunk.ChunkIndex))
			embeddings = append(embeddings, encoded[i][j])
		}
	}
	if len(accepted) == 0 {
		return stale, nil
	}
	q = fmt.Sprintf(`INSERT INTO %s (generation_id,doc_key,revision) SELECT $1,doc_key,revision FROM unnest(ARRAY(SELECT %s FROM %s WHERE false) || $2, ARRAY(SELECT %s FROM %s WHERE false) || $3) AS input(doc_key,revision) ORDER BY doc_key ON CONFLICT (generation_id,doc_key) DO UPDATE SET revision=excluded.revision`, s.stamps(), key, table, rev, table)
	if _, err = tx.ExecContext(ctx, q, row.id, accepted, acceptedRevisions); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE doc_key=ANY($1)`, s.chunks(row.id)), accepted); err != nil {
		return nil, err
	}
	if len(chunkKeys) > 0 {
		q = fmt.Sprintf(`INSERT INTO %s (doc_key,chunk_index,embedding) SELECT doc_key,chunk_index,embedding::%s.halfvec FROM unnest(ARRAY(SELECT %s FROM %s WHERE false) || $1,$2::bigint[],$3::text[]) AS chunks(doc_key,chunk_index,embedding)`, s.chunks(row.id), postgres.QuoteIdentifier(s.ext), key, table)
		if _, err = tx.ExecContext(ctx, q, chunkKeys, indexes, embeddings); err != nil {
			return nil, err
		}
	}
	return stale, nil
}
