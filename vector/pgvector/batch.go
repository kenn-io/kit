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
// Each document key must appear at most once.
func (s *Store[K, G]) SaveDocuments(ctx context.Context, tx *sql.Tx, gen G, docs []Document[K]) ([]K, error) {
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
	for i, doc := range docs {
		if seen[doc.Doc] {
			return nil, fmt.Errorf("duplicate document %v", doc.Doc)
		}
		seen[doc.Doc] = true
		keys[i], revisions[i] = doc.Doc, doc.Revision
	}
	key, rev, table := postgres.QuoteIdentifier(s.schema.IDColumn), postgres.QuoteIdentifier(s.schema.RevisionColumn), postgres.QuoteIdentifier(s.schema.DocsTable)
	q := fmt.Sprintf(`SELECT d.%s FROM %s d JOIN unnest(ARRAY(SELECT %s FROM %s WHERE false) || $1, ARRAY(SELECT %s FROM %s WHERE false) || $2) AS input(doc_key,revision) ON d.%s=input.doc_key WHERE d.%s IS NOT DISTINCT FROM input.revision FOR SHARE OF d`, key, table, key, table, rev, table, key, rev)
	rows, err := tx.QueryContext(ctx, q, keys, revisions)
	if err != nil {
		return nil, err
	}
	current := make(map[K]bool, len(docs))
	for rows.Next() {
		var key K
		if err := rows.Scan(&key); err != nil {
			_ = rows.Close()
			return nil, err
		}
		current[key] = true
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	var stale, accepted, chunkKeys []K
	var indexes []int64
	var embeddings []string
	for _, doc := range docs {
		if !current[doc.Doc] {
			stale = append(stale, doc.Doc)
			continue
		}
		accepted = append(accepted, doc.Doc)
		for _, chunk := range doc.Vectors {
			if len(chunk.Vector) != row.dim {
				return nil, fmt.Errorf("chunk %d has %d dimensions, expected %d", chunk.ChunkIndex, len(chunk.Vector), row.dim)
			}
			embedding, err := halfvec(chunk.Vector)
			if err != nil {
				return nil, fmt.Errorf("chunk %d: %w", chunk.ChunkIndex, err)
			}
			chunkKeys = append(chunkKeys, doc.Doc)
			indexes = append(indexes, int64(chunk.ChunkIndex))
			embeddings = append(embeddings, embedding)
		}
	}
	if len(accepted) == 0 {
		return stale, nil
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
	q = fmt.Sprintf(`INSERT INTO %s (generation_id,doc_key,revision) SELECT $1,%s,%s FROM %s WHERE %s=ANY($2) ON CONFLICT (generation_id,doc_key) DO UPDATE SET revision=excluded.revision`, s.stamps(), key, rev, table, key)
	if _, err = tx.ExecContext(ctx, q, row.id, accepted); err != nil {
		return nil, err
	}
	return stale, nil
}
