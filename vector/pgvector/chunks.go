package pgvector

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"iter"
	"slices"
	"strings"

	"go.kenn.io/kit/search/postgres"
	"go.kenn.io/kit/search/sqlquery"
	"go.kenn.io/kit/vector"
)

// Chunks accesses an application-owned table of halfvec chunks.
type Chunks[K comparable] struct {
	table, ext string
}

// Chunk is one document's vector at a chunk index.
type Chunk[K comparable] struct {
	Doc        K
	ChunkIndex int
	Vector     vector.Vector
}

// AttachChunks resolves a table on the queryer's search path without changing it.
// Keys must be supported by database/sql for both binding and scanning.
func AttachChunks[K comparable](ctx context.Context, q sqlquery.Queryer, table string) (*Chunks[K], error) {
	query := sqlquery.Query{SQL: `SELECT n.nspname, c.relname, en.nspname
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
JOIN pg_attribute a ON a.attrelid=c.oid AND a.attname='embedding' AND NOT a.attisdropped
JOIN pg_type t ON t.oid=a.atttypid
JOIN pg_depend d ON d.classid='pg_type'::regclass AND d.objid=t.oid AND d.refclassid='pg_extension'::regclass AND d.deptype='e'
JOIN pg_extension e ON e.oid=d.refobjid AND e.extname='vector'
JOIN pg_namespace en ON en.oid=e.extnamespace
WHERE c.oid=to_regclass($1) AND t.typname='halfvec'`, Args: []any{postgres.QuoteIdentifier(table)}}
	type tableNames struct{ namespace, name, ext string }
	names, err := query.All(ctx, q, func(rows *sql.Rows) (tableNames, error) {
		var names tableNames
		err := rows.Scan(&names.namespace, &names.name, &names.ext)
		return names, err
	})
	if err != nil {
		return nil, fmt.Errorf("resolve chunk table: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("pgvector: table %q has no extension-owned halfvec embedding", table)
	}
	qualified := postgres.QuoteIdentifier(names[0].namespace) + "." + postgres.QuoteIdentifier(names[0].name)
	return &Chunks[K]{table: qualified, ext: postgres.QuoteIdentifier(names[0].ext)}, nil
}

// ReplaceTx deletes the selected documents' chunks and streams replacements.
// Roll back tx on error. A nil sequence clears the selected documents.
func (c *Chunks[K]) ReplaceTx(ctx context.Context, tx *sql.Tx, keys []K, chunks iter.Seq2[Chunk[K], error]) (int, error) {
	for start := 0; start < len(keys); start += 200 {
		batch := keys[start:min(start+200, len(keys))]
		args := make([]any, len(batch))
		placeholders := make([]string, len(batch))
		for i, key := range batch {
			args[i] = key
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+c.table+" WHERE doc_key IN ("+strings.Join(placeholders, ",")+")", args...); err != nil {
			return 0, fmt.Errorf("delete chunks: %w", err)
		}
	}
	if chunks == nil {
		return 0, nil
	}
	return c.insertChunks(ctx, tx, keys, chunks)
}

// insertChunks streams chunks in bounded batches and rejects documents outside keys.
func (c *Chunks[K]) insertChunks(ctx context.Context, tx *sql.Tx, keys []K, chunks iter.Seq2[Chunk[K], error]) (int, error) {
	selected := make(map[K]struct{}, len(keys))
	for _, key := range keys {
		selected[key] = struct{}{}
	}
	var args []any
	var values []string
	count := 0
	flush := func() error {
		if len(values) == 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO "+c.table+" (doc_key,chunk_index,embedding) VALUES "+strings.Join(values, ","), args...)
		args, values = nil, nil
		return err
	}
	for chunk, err := range chunks {
		if err != nil {
			return 0, fmt.Errorf("read chunks: %w", err)
		}
		if _, ok := selected[chunk.Doc]; !ok {
			return 0, fmt.Errorf("pgvector: chunk %d of document %v is outside the selected keys", chunk.ChunkIndex, chunk.Doc)
		}
		literal, err := halfvec(chunk.Vector)
		if err != nil {
			return 0, fmt.Errorf("document %v chunk %d: %w", chunk.Doc, chunk.ChunkIndex, err)
		}
		n := len(args)
		values = append(values, fmt.Sprintf("($%d,$%d,$%d::%s.halfvec)", n+1, n+2, n+3, c.ext))
		args = append(args, chunk.Doc, chunk.ChunkIndex, literal)
		count++
		if len(values) == 200 {
			if err := flush(); err != nil {
				return 0, fmt.Errorf("insert chunks: %w", err)
			}
		}
	}
	if err := flush(); err != nil {
		return 0, fmt.Errorf("insert chunks: %w", err)
	}
	return count, nil
}

// SearchTx returns raw nearest chunks. The caller owns freshness, hydration,
// exhaustion decisions, and transaction completion.
func (c *Chunks[K]) SearchTx(ctx context.Context, tx *sql.Tx, query vector.Vector, limit int) ([]vector.Hit[K], error) {
	literal, err := halfvec(query)
	if err != nil {
		return nil, fmt.Errorf("query vector: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", min(max(limit, 40), 1000))); err != nil {
		return nil, fmt.Errorf("set hnsw.ef_search: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL hnsw.iterative_scan = strict_order`); err != nil {
		return nil, fmt.Errorf("set hnsw.iterative_scan: %w", err)
	}
	distance := fmt.Sprintf("embedding OPERATOR(%s.<=>) $1::%s.halfvec", c.ext, c.ext)
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("SELECT doc_key,NULL,1-(%s),chunk_index FROM %s ORDER BY %s LIMIT $2", distance, c.table, distance), literal, limit)
	if err != nil {
		return nil, fmt.Errorf("search chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	hits, err := scanHits[K](rows)
	if err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close chunks: %w", err)
	}
	slices.SortStableFunc(hits, func(a, b vector.Hit[K]) int { return cmp.Compare(b.Score, a.Score) })
	return hits, nil
}
