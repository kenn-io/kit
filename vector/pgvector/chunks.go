package pgvector

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
	"math"
	"strings"

	"go.kenn.io/kit/search/postgres"
	"go.kenn.io/kit/search/sqlquery"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/internal/vectorliteral"
)

// Chunks accesses an application-owned table of halfvec chunks.
type Chunks[K comparable] struct {
	table, ext string
	dimension  int
}

// Chunk is one document's vector at a chunk index.
type Chunk[K comparable] struct {
	Doc        K
	ChunkIndex int
	Vector     vector.Vector
}

// ChunkPage contains raw nearest chunks and the scan's maximum row count.
type ChunkPage[K comparable] struct {
	Hits    []vector.Hit[K]
	ScanCap int
}

// AttachChunks resolves a table on the queryer's search path without changing it.
// Keys must be supported by database/sql for both binding and scanning.
func AttachChunks[K comparable](ctx context.Context, q sqlquery.Queryer, table string, dimension int) (*Chunks[K], error) {
	if dimension <= 0 {
		return nil, fmt.Errorf("pgvector: invalid dimension %d", dimension)
	}
	rows, err := q.QueryContext(ctx, `SELECT n.nspname, c.relname, en.nspname, a.atttypmod
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
JOIN pg_attribute a ON a.attrelid=c.oid AND a.attname='embedding' AND NOT a.attisdropped
JOIN pg_type t ON t.oid=a.atttypid
JOIN pg_depend d ON d.classid='pg_type'::regclass AND d.objid=t.oid AND d.refclassid='pg_extension'::regclass AND d.deptype='e'
JOIN pg_extension e ON e.oid=d.refobjid AND e.extname='vector'
JOIN pg_namespace en ON en.oid=e.extnamespace
WHERE c.oid=to_regclass($1) AND t.typname='halfvec'`, postgres.QuoteIdentifier(table))
	if err != nil {
		return nil, fmt.Errorf("resolve chunk table: %w", err)
	}
	var namespace, name, ext string
	var dim int
	if !rows.Next() {
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, fmt.Errorf("resolve chunk table: %w", err)
		}
		return nil, fmt.Errorf("pgvector: table %q has no extension-owned halfvec embedding", table)
	}
	err = rows.Scan(&namespace, &name, &ext, &dim)
	if err == nil {
		err = rows.Err()
	}
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, fmt.Errorf("resolve chunk table: %w", err)
	}
	if dim != dimension {
		return nil, fmt.Errorf("pgvector: table %q has dimension %d, expected %d", table, dim, dimension)
	}
	qualified := postgres.QuoteIdentifier(namespace) + "." + postgres.QuoteIdentifier(name)
	rows, err = q.QueryContext(ctx, "SELECT doc_key,chunk_index,embedding FROM "+qualified+" WHERE false")
	if err != nil {
		return nil, fmt.Errorf("check chunk table: %w", err)
	}
	err = rows.Close()
	if err != nil {
		return nil, fmt.Errorf("check chunk table: %w", err)
	}
	return &Chunks[K]{table: qualified, ext: postgres.QuoteIdentifier(ext), dimension: dimension}, nil
}

func (c *Chunks[K]) literal(v vector.Vector) (string, error) {
	if len(v) != c.dimension {
		return "", fmt.Errorf("pgvector: vector has %d dimensions, expected %d", len(v), c.dimension)
	}
	for i, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return "", fmt.Errorf("pgvector: vector component %d is non-finite", i)
		}
	}
	return vectorliteral.Format(v), nil
}

// ReplaceTx deletes the selected documents' chunks and streams replacements.
// Roll back tx on error. A nil sequence clears the selected documents.
func (c *Chunks[K]) ReplaceTx(ctx context.Context, tx *sql.Tx, keys []K, chunks iter.Seq2[Chunk[K], error]) (int, error) {
	selected := make(map[K]struct{}, len(keys))
	for _, key := range keys {
		selected[key] = struct{}{}
	}
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
			return 0, fmt.Errorf("pgvector: chunk key %v was not selected", chunk.Doc)
		}
		literal, err := c.literal(chunk.Vector)
		if err != nil {
			return 0, fmt.Errorf("chunk %d: %w", chunk.ChunkIndex, err)
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
func (c *Chunks[K]) SearchTx(ctx context.Context, tx *sql.Tx, query vector.Vector, limit int) (ChunkPage[K], error) {
	var page ChunkPage[K]
	literal, err := c.literal(query)
	if err != nil {
		return page, fmt.Errorf("query embedding: %w", err)
	}
	k := max(limit, 0)
	ef := min(max(k, 40), 1000)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", ef)); err != nil {
		return page, fmt.Errorf("set hnsw.ef_search: %w", err)
	}
	page.ScanCap = k
	if k > ef {
		var setting sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT current_setting('hnsw.iterative_scan', true)`).Scan(&setting); err != nil {
			return page, fmt.Errorf("probe hnsw.iterative_scan: %w", err)
		}
		if !setting.Valid {
			page.ScanCap = ef
		} else if _, err := tx.ExecContext(ctx, `SET LOCAL hnsw.iterative_scan = 'relaxed_order'`); err != nil {
			return page, fmt.Errorf("set hnsw.iterative_scan: %w", err)
		}
	}
	distance := fmt.Sprintf("embedding OPERATOR(%s.<=>) $1::%s.halfvec", c.ext, c.ext)
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("SELECT doc_key,chunk_index,1-(%s) FROM %s ORDER BY %s LIMIT $2", distance, c.table, distance), literal, k)
	if err != nil {
		return page, fmt.Errorf("search chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var hit vector.Hit[K]
		var score float64
		if err := rows.Scan(&hit.Doc, &hit.ChunkIndex, &score); err != nil {
			return page, fmt.Errorf("scan chunk: %w", err)
		}
		hit.Score = float32(score)
		page.Hits = append(page.Hits, hit)
	}
	if err := rows.Err(); err != nil {
		return page, fmt.Errorf("iterate chunks: %w", err)
	}
	if err := rows.Close(); err != nil {
		return page, fmt.Errorf("close chunks: %w", err)
	}
	return page, nil
}
