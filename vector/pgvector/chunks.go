package pgvector

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
	"strings"

	"go.kenn.io/kit/search/postgres"
	"go.kenn.io/kit/search/sqlquery"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/internal/vectorliteral"
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

// ChunkPage contains raw hits and the maximum scan size used for the request.
type ChunkPage[K comparable] struct {
	Hits    []vector.Hit[K]
	ScanCap int
}

// AttachChunks resolves a table on the queryer's search path without changing it.
// Keys must be supported by database/sql for both binding and scanning.
func AttachChunks[K comparable](ctx context.Context, q sqlquery.Queryer, table string) (*Chunks[K], error) {
	query := sqlquery.Query{SQL: `SELECT n.nspname
FROM pg_attribute a JOIN pg_type t ON t.oid=a.atttypid
JOIN pg_namespace n ON n.oid=t.typnamespace
WHERE a.attrelid=to_regclass($1) AND a.attname='embedding' AND NOT a.attisdropped AND t.typname='halfvec'`, Args: []any{postgres.QuoteIdentifier(table)}}
	names, err := query.All(ctx, q, func(rows *sql.Rows) (string, error) {
		var name string
		err := rows.Scan(&name)
		return name, err
	})
	if err != nil {
		return nil, fmt.Errorf("resolve chunk table: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("pgvector: table %q has no halfvec embedding", table)
	}
	return &Chunks[K]{table: postgres.QuoteIdentifier(table), ext: postgres.QuoteIdentifier(names[0])}, nil
}

// ReplaceTx deletes the selected documents' chunks and streams replacements.
// Roll back tx on error. A nil sequence clears the selected documents.
// Callers must serialize replacements for each document through commit.
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
		n := len(args)
		values = append(values, fmt.Sprintf("($%d,$%d,$%d::%s.halfvec)", n+1, n+2, n+3, c.ext))
		args = append(args, chunk.Doc, chunk.ChunkIndex, vectorliteral.Format(chunk.Vector))
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
	limit = max(limit, 0)
	efSearch := min(max(limit, 40), 1000)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", efSearch)); err != nil {
		return page, fmt.Errorf("set hnsw.ef_search: %w", err)
	}
	page.ScanCap = limit
	if limit > efSearch {
		var setting sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT current_setting('hnsw.iterative_scan', true)`).Scan(&setting); err != nil {
			return page, fmt.Errorf("probe hnsw.iterative_scan: %w", err)
		}
		if !setting.Valid {
			page.ScanCap = efSearch
		} else if _, err := tx.ExecContext(ctx, `SET LOCAL hnsw.iterative_scan = 'relaxed_order'`); err != nil {
			return page, fmt.Errorf("set hnsw.iterative_scan: %w", err)
		}
	}
	distance := fmt.Sprintf("embedding OPERATOR(%s.<=>) $1::%s.halfvec", c.ext, c.ext)
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("SELECT doc_key,chunk_index,1-(%s) FROM %s ORDER BY %s LIMIT $2", distance, c.table, distance), vectorliteral.Format(query), limit)
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
