package pgvector

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"iter"
	"math"
	"reflect"
	"strings"

	"go.kenn.io/kit/search/postgres"
	"go.kenn.io/kit/search/sqlquery"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/internal/vectorliteral"
)

// Chunks accesses an application-owned table of halfvec chunks.
type Chunks[K comparable] struct {
	table, ext, keyType string
	dimension           int
}

// Chunk is one document's vector at a chunk index.
type Chunk[K comparable] struct {
	Doc        K
	ChunkIndex int
	Vector     vector.Vector
}

// ChunkPage contains raw hits and the configured scan cap. Compare ScanCap
// with the raw hit count, before rolling up documents. Approximate scans may
// return short pages even when more matches exist.
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
	query := sqlquery.Query{SQL: `SELECT n.nspname, c.relname, en.nspname, pg_catalog.format_type(k.atttypid,k.atttypmod)
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
JOIN pg_attribute k ON k.attrelid=c.oid AND k.attname='doc_key' AND NOT k.attisdropped
JOIN pg_attribute a ON a.attrelid=c.oid AND a.attname='embedding' AND NOT a.attisdropped
JOIN pg_type t ON t.oid=a.atttypid
JOIN pg_depend d ON d.classid='pg_type'::regclass AND d.objid=t.oid AND d.refclassid='pg_extension'::regclass AND d.deptype='e'
JOIN pg_extension e ON e.oid=d.refobjid AND e.extname='vector'
JOIN pg_namespace en ON en.oid=e.extnamespace
WHERE c.oid=to_regclass($1) AND t.typname='halfvec' AND a.atttypmod=$2`, Args: []any{postgres.QuoteIdentifier(table), dimension}}
	type tableNames struct{ namespace, name, ext, keyType string }
	names, err := query.All(ctx, q, func(rows *sql.Rows) (tableNames, error) {
		var names tableNames
		err := rows.Scan(&names.namespace, &names.name, &names.ext, &names.keyType)
		return names, err
	})
	if err != nil {
		return nil, fmt.Errorf("resolve chunk table: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("pgvector: table %q has no extension-owned halfvec embedding", table)
	}
	qualified := postgres.QuoteIdentifier(names[0].namespace) + "." + postgres.QuoteIdentifier(names[0].name)
	if err := checkTable(ctx, q, qualified, "doc_key,chunk_index,embedding"); err != nil {
		return nil, fmt.Errorf("check chunk table: %w", err)
	}
	return &Chunks[K]{table: qualified, ext: postgres.QuoteIdentifier(names[0].ext), dimension: dimension, keyType: names[0].keyType}, nil
}

// ReplaceTx deletes the selected documents' chunks and streams replacements.
// Roll back tx on error. A nil sequence clears the selected documents.
// Callers must serialize replacements for each document through commit.
func (c *Chunks[K]) ReplaceTx(ctx context.Context, tx *sql.Tx, keys []K, chunks iter.Seq2[Chunk[K], error]) (int, error) {
	for _, key := range keys {
		if _, err := comparableKey(key); err != nil {
			return 0, err
		}
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
	return c.insertChunks(ctx, tx, keys, chunks)
}

// insertChunks streams chunks in bounded batches and rejects documents outside keys.
func (c *Chunks[K]) insertChunks(ctx context.Context, tx *sql.Tx, keys []K, chunks iter.Seq2[Chunk[K], error]) (int, error) {
	selected := make(map[any]struct{}, len(keys))
	for _, key := range keys {
		k, _ := comparableKey(key)
		selected[k] = struct{}{}
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
		key, err := comparableKey(chunk.Doc)
		if err != nil {
			return 0, err
		}
		if _, ok := selected[key]; !ok {
			matched, err := c.matchesKey(ctx, tx, chunk.Doc, keys)
			if err != nil {
				return 0, err
			}
			if !matched {
				return 0, fmt.Errorf("pgvector: chunk %d of document %v is outside the selected keys", chunk.ChunkIndex, chunk.Doc)
			}
		}
		literal, err := c.literal(chunk.Vector)
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

// comparableKey normalizes database/sql values without hashing slices.
func comparableKey(key any) (any, error) {
	value, err := driver.DefaultParameterConverter.ConvertValue(key)
	if err != nil {
		return nil, fmt.Errorf("pgvector: unsupported document key: %w", err)
	}
	if b, ok := value.([]byte); ok {
		return struct{ binary string }{string(b)}, nil
	}
	if value != nil && !reflect.TypeOf(value).Comparable() {
		return nil, fmt.Errorf("pgvector: unsupported document key type %T", value)
	}
	return value, nil
}

// matchesKey uses the column's equality for alternate database representations.
func (c *Chunks[K]) matchesKey(ctx context.Context, tx *sql.Tx, key K, keys []K) (bool, error) {
	for start := 0; start < len(keys); start += 200 {
		batch := keys[start:min(start+200, len(keys))]
		args := []any{key}
		placeholders := make([]string, len(batch))
		for i, selected := range batch {
			args = append(args, selected)
			placeholders[i] = fmt.Sprintf("$%d::%s", i+2, c.keyType)
		}
		var matched bool
		query := fmt.Sprintf("SELECT COALESCE($1::%s IN (%s), false)", c.keyType, strings.Join(placeholders, ","))
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&matched); err != nil {
			return false, fmt.Errorf("compare document keys: %w", err)
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

// literal validates dimensions and finite components without changing stored values.
func (c *Chunks[K]) literal(v vector.Vector) (string, error) {
	if len(v) != c.dimension {
		return "", fmt.Errorf("pgvector: vector dimension %d, want %d", len(v), c.dimension)
	}
	// Half precision rounds values at or below half its smallest subnormal to zero.
	var nonzero bool
	for i, component := range v {
		if math.IsNaN(float64(component)) || math.IsInf(float64(component), 0) {
			return "", fmt.Errorf("pgvector: vector component %d is non-finite", i)
		}
		nonzero = nonzero || math.Abs(float64(component)) > 1.0/(1<<25)
	}
	if !nonzero {
		return "", errors.New("pgvector: vector is zero at halfvec precision")
	}
	return vectorliteral.Format(v), nil
}

// SearchTx returns raw nearest chunks. The caller owns freshness, hydration,
// exhaustion decisions, and transaction completion. A short approximate page
// alone does not prove the scan found all matches.
func (c *Chunks[K]) SearchTx(ctx context.Context, tx *sql.Tx, query vector.Vector, limit int) (ChunkPage[K], error) {
	var page ChunkPage[K]
	literal, err := c.literal(query)
	if err != nil {
		return page, fmt.Errorf("query vector: %w", err)
	}
	limit = max(limit, 0)
	// Loading the type registers pgvector's session settings on this connection.
	var loaded bool
	if err := tx.QueryRowContext(ctx, "SELECT $1::"+c.ext+".halfvec IS NOT NULL", literal).Scan(&loaded); err != nil {
		return page, fmt.Errorf("load pgvector: %w", err)
	}
	var previousEF string
	var previousIterative sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT current_setting('hnsw.ef_search'), current_setting('hnsw.iterative_scan', true)`).Scan(&previousEF, &previousIterative); err != nil {
		return page, fmt.Errorf("read hnsw settings: %w", err)
	}
	efSearch := min(max(limit, 40), 1000)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", efSearch)); err != nil {
		return page, fmt.Errorf("set hnsw.ef_search: %w", err)
	}
	page.ScanCap = limit
	if limit > efSearch {
		if !previousIterative.Valid {
			page.ScanCap = efSearch
		} else if _, err := tx.ExecContext(ctx, `SET LOCAL hnsw.iterative_scan = 'relaxed_order'`); err != nil {
			return page, fmt.Errorf("set hnsw.iterative_scan: %w", err)
		}
	}
	// PostgreSQL 17 needs + 0 to sort relaxed HNSW results after the candidate limit.
	distance := fmt.Sprintf("embedding OPERATOR(%s.<=>) $1::%s.halfvec", c.ext, c.ext)
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("WITH candidates AS MATERIALIZED (SELECT doc_key,chunk_index,%s AS distance FROM %s WHERE embedding IS NOT NULL AND %s.l2_norm(embedding) > 0 ORDER BY %s LIMIT $2) SELECT doc_key,chunk_index,1-distance FROM candidates ORDER BY distance + 0", distance, c.table, c.ext, distance), literal, limit)
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
	if _, err := tx.ExecContext(ctx, `SELECT set_config('hnsw.ef_search', $1, true)`, previousEF); err != nil {
		return page, fmt.Errorf("restore hnsw.ef_search: %w", err)
	}
	if limit > efSearch && previousIterative.Valid {
		if _, err := tx.ExecContext(ctx, `SELECT set_config('hnsw.iterative_scan', $1, true)`, previousIterative.String); err != nil {
			return page, fmt.Errorf("restore hnsw.iterative_scan: %w", err)
		}
	}
	return page, nil
}
