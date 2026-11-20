package pgvector

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"go.kenn.io/kit/search/postgres"
	"go.kenn.io/kit/search/sqlquery"
	"go.kenn.io/kit/vector"
)

// Chunks accesses an application-owned table of halfvec chunks.
type Chunks[K comparable] struct {
	table, ext, keyType, keyCollation string
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
	query := sqlquery.Query{SQL: `SELECT n.nspname, c.relname, en.nspname, kn.nspname, kt.typname, COALESCE(cn.nspname, ''), COALESCE(co.collname, '')
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
JOIN pg_attribute k ON k.attrelid=c.oid AND k.attname='doc_key' AND NOT k.attisdropped
JOIN pg_type kt ON kt.oid=k.atttypid JOIN pg_namespace kn ON kn.oid=kt.typnamespace
LEFT JOIN pg_collation co ON co.oid=k.attcollation LEFT JOIN pg_namespace cn ON cn.oid=co.collnamespace
JOIN pg_attribute p ON p.attrelid=c.oid AND p.attname='chunk_index' AND NOT p.attisdropped
JOIN pg_attribute a ON a.attrelid=c.oid AND a.attname='embedding' AND NOT a.attisdropped
JOIN pg_type t ON t.oid=a.atttypid JOIN pg_namespace en ON en.oid=t.typnamespace
JOIN pg_depend d ON d.classid='pg_type'::regclass AND d.objid=t.oid AND d.refclassid='pg_extension'::regclass AND d.deptype='e'
JOIN pg_extension e ON e.oid=d.refobjid AND e.extname='vector'
WHERE c.oid=to_regclass($1) AND t.typname='halfvec'`, Args: []any{postgres.QuoteIdentifier(table)}}
	type tableNames struct{ schema, name, ext, keySchema, keyType, colSchema, collation string }
	names, err := query.All(ctx, q, func(rows *sql.Rows) (tableNames, error) {
		var name tableNames
		err := rows.Scan(&name.schema, &name.name, &name.ext, &name.keySchema, &name.keyType, &name.colSchema, &name.collation)
		return name, err
	})
	if err != nil {
		return nil, fmt.Errorf("resolve chunk table: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("pgvector: table %q has no halfvec embedding", table)
	}
	name := names[0]
	var collation string
	if name.collation != "" {
		collation = " COLLATE " + postgres.QuoteIdentifier(name.colSchema) + "." + postgres.QuoteIdentifier(name.collation)
	}
	return &Chunks[K]{table: postgres.QuoteIdentifier(name.schema) + "." + postgres.QuoteIdentifier(name.name), ext: postgres.QuoteIdentifier(name.ext), keyType: postgres.QuoteIdentifier(name.keySchema) + "." + postgres.QuoteIdentifier(name.keyType), keyCollation: collation}, nil
}

// ReplaceTx deletes the selected documents' chunks and streams replacements.
// Roll back tx on error. A nil sequence clears the selected documents.
// Callers must serialize replacements for each document through commit.
func (c *Chunks[K]) ReplaceTx(ctx context.Context, tx *sql.Tx, keys []K, chunks iter.Seq2[Chunk[K], error]) (int, error) {
	selected := make([]any, len(keys))
	for i, key := range keys {
		value, err := chunkKey(key)
		if err != nil {
			return 0, fmt.Errorf("document key: %w", err)
		}
		selected[i] = value
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM "+c.table+" WHERE doc_key = ANY($1)", selected); err != nil {
		return 0, fmt.Errorf("delete chunks: %w", err)
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
		// Check again after insertion because column precision can change a key.
		query := "WITH inserted AS (INSERT INTO " + c.table + " (doc_key,chunk_index,embedding) SELECT v.doc_key,v.chunk_index,v.embedding FROM (VALUES " + strings.Join(values, ",") + ") AS v(doc_key,chunk_index,embedding) WHERE v.doc_key" + c.keyCollation + " = ANY($1::" + c.keyType + "[]) RETURNING doc_key) SELECT count(*), count(*) FILTER (WHERE doc_key" + c.keyCollation + " = ANY($1::" + c.keyType + "[])) FROM inserted"
		var inserted, matched int
		if err := tx.QueryRowContext(ctx, query, append([]any{selected}, args...)...).Scan(&inserted, &matched); err != nil {
			return err
		}
		if matched != len(values) || inserted != len(values) {
			return errors.New("pgvector: chunk document is outside the selected keys")
		}
		args, values = nil, nil
		return nil
	}
	for chunk, err := range chunks {
		if err != nil {
			return 0, fmt.Errorf("read chunks: %w", err)
		}
		literal, err := halfvec(chunk.Vector)
		if err != nil {
			return 0, fmt.Errorf("document %v chunk %d: %w", chunk.Doc, chunk.ChunkIndex, err)
		}
		key, err := chunkKey(chunk.Doc)
		if err != nil {
			return 0, fmt.Errorf("document key: %w", err)
		}
		n := len(args) + 1
		values = append(values, fmt.Sprintf("($%d::%s,$%d::bigint,$%d::%s.halfvec)", n+1, c.keyType, n+2, n+3, c.ext))
		args = append(args, key, chunk.ChunkIndex, literal)
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

func chunkKey(key any) (driver.Value, error) {
	value, err := driver.DefaultParameterConverter.ConvertValue(key)
	if b, ok := value.([]byte); ok {
		value = bytes.Clone(b)
	}
	return value, err
}

// SearchTx returns raw nearest chunks. The caller owns freshness, hydration,
// exhaustion decisions, and transaction completion. Roll back tx on error.
func (c *Chunks[K]) SearchTx(ctx context.Context, tx *sql.Tx, query vector.Vector, limit int) (page ChunkPage[K], resultErr error) {
	literal, err := halfvec(query)
	if err != nil {
		return page, err
	}
	// Loading the type registers pgvector's transaction-local settings.
	var loaded bool
	if err := tx.QueryRowContext(ctx, "SELECT $1::"+c.ext+".halfvec IS NOT NULL", literal).Scan(&loaded); err != nil {
		return page, fmt.Errorf("load pgvector: %w", err)
	}
	var previousEF, previousIterative string
	if err := tx.QueryRowContext(ctx, `SELECT current_setting('hnsw.ef_search'), current_setting('hnsw.iterative_scan')`).Scan(&previousEF, &previousIterative); err != nil {
		return page, fmt.Errorf("read hnsw settings: %w", err)
	}
	limit = max(limit, 0)
	efSearch := min(max(limit, 40), 1000)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", efSearch)); err != nil {
		return page, fmt.Errorf("set hnsw.ef_search: %w", err)
	}
	defer func() {
		// Restore settings even if the search context expired while tx remains live.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := tx.ExecContext(cleanupCtx, `SELECT set_config('hnsw.ef_search', $1, true)`, previousEF); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("restore hnsw.ef_search: %w", err))
		}
		if limit > efSearch {
			if _, err := tx.ExecContext(cleanupCtx, `SELECT set_config('hnsw.iterative_scan', $1, true)`, previousIterative); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("restore hnsw.iterative_scan: %w", err))
			}
		}
	}()
	page.ScanCap = limit
	if limit > efSearch {
		if _, err := tx.ExecContext(ctx, `SET LOCAL hnsw.iterative_scan = 'relaxed_order'`); err != nil {
			return page, fmt.Errorf("set hnsw.iterative_scan: %w", err)
		}
	}
	distance := fmt.Sprintf("embedding OPERATOR(%s.<=>) $1::%s.halfvec", c.ext, c.ext)
	q := sqlquery.Query{SQL: fmt.Sprintf("WITH candidates AS MATERIALIZED (SELECT doc_key,chunk_index,%s AS distance FROM %s WHERE embedding IS NOT NULL AND %s.l2_norm(embedding) > 0 ORDER BY %s LIMIT $2) SELECT doc_key,chunk_index,1-distance FROM candidates ORDER BY distance + 0", distance, c.table, c.ext, distance), Args: []any{literal, page.ScanCap}}
	hits, err := q.All(ctx, tx, func(rows *sql.Rows) (vector.Hit[K], error) {
		var hit vector.Hit[K]
		var score float64
		if err := rows.Scan(&hit.Doc, &hit.ChunkIndex, &score); err != nil {
			return hit, err
		}
		hit.Score = float32(score)
		return hit, nil
	})
	if err != nil {
		return page, fmt.Errorf("search chunks: %w", err)
	}
	page.Hits = hits
	return page, nil
}
