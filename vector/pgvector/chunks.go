package pgvector

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"iter"
	"math"
	"reflect"
	"strings"
	"time"

	"go.kenn.io/kit/search/postgres"
	"go.kenn.io/kit/search/sqlquery"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/internal/vectorliteral"
)

// Chunks accesses an application-owned table of halfvec chunks.
type Chunks[K comparable] struct {
	table, ext, keyBaseType, keyColumnType, keyCollation, keyEqual string
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

// AttachChunks resolves a table on the queryer's search path. Callers must use
// a trusted search path when attaching and using Chunks.
// The table must have non-null text or bigint doc_key and non-null integer or
// bigint chunk_index columns, and a fixed-dimension halfvec embedding column.
// Keys must be supported by database/sql for both binding and scanning.
func AttachChunks[K comparable](ctx context.Context, q sqlquery.Queryer, table string) (*Chunks[K], error) {
	query := sqlquery.Query{SQL: `SELECT n.nspname, c.relname, en.nspname, bn.nspname, pg_catalog.format('%I.%I', bn.nspname, bt.typname),
CASE WHEN k.atttypmod = -1 THEN pg_catalog.format('%I.%I', kn.nspname, kt.typname)
     WHEN kn.nspname <> 'pg_catalog' THEN pg_catalog.format('%I.%I', kn.nspname, kt.typname) || pg_catalog.substr(pg_catalog.format_type(k.atttypid, k.atttypmod), pg_catalog.length(pg_catalog.format_type(k.atttypid, -1)) + 1)
     ELSE pg_catalog.format_type(k.atttypid, k.atttypmod) END, COALESCE(cn.nspname, ''), COALESCE(co.collname, ''),
EXISTS (SELECT 1 FROM pg_catalog.pg_operator op WHERE op.oprnamespace=bn.oid AND op.oprname='=' AND op.oprleft=bt.oid AND op.oprright=bt.oid)
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
JOIN pg_catalog.pg_attribute k ON k.attrelid=c.oid AND k.attname='doc_key' AND NOT k.attisdropped
JOIN pg_catalog.pg_type kt ON kt.oid=k.atttypid JOIN pg_catalog.pg_namespace kn ON kn.oid=kt.typnamespace
JOIN LATERAL (WITH RECURSIVE base AS (
  SELECT kt.oid, kt.typbasetype
  UNION ALL SELECT t.oid, t.typbasetype FROM pg_catalog.pg_type t JOIN base b ON t.oid=b.typbasetype
) SELECT oid FROM base WHERE typbasetype=0) b ON true
JOIN pg_catalog.pg_type bt ON bt.oid=b.oid JOIN pg_catalog.pg_namespace bn ON bn.oid=bt.typnamespace
LEFT JOIN pg_catalog.pg_collation co ON co.oid=k.attcollation LEFT JOIN pg_catalog.pg_namespace cn ON cn.oid=co.collnamespace
JOIN pg_catalog.pg_attribute p ON p.attrelid=c.oid AND p.attname='chunk_index' AND NOT p.attisdropped
JOIN pg_catalog.pg_attribute a ON a.attrelid=c.oid AND a.attname='embedding' AND NOT a.attisdropped
JOIN pg_catalog.pg_type t ON t.oid=a.atttypid JOIN pg_catalog.pg_namespace en ON en.oid=t.typnamespace
JOIN pg_catalog.pg_depend d ON d.classid='pg_catalog.pg_type'::pg_catalog.regclass AND d.objid=t.oid AND d.refclassid='pg_catalog.pg_extension'::pg_catalog.regclass AND d.deptype='e'
JOIN pg_catalog.pg_extension e ON e.oid=d.refobjid AND e.extname='vector'
WHERE c.oid=pg_catalog.to_regclass($1) AND t.typname='halfvec' AND a.atttypmod > 0
AND k.attnotnull AND k.atttypid IN ('pg_catalog.text'::pg_catalog.regtype, 'pg_catalog.int8'::pg_catalog.regtype)
AND p.attnotnull AND p.atttypid IN ('pg_catalog.int4'::pg_catalog.regtype, 'pg_catalog.int8'::pg_catalog.regtype)`, Args: []any{postgres.QuoteIdentifier(table)}}
	type tableNames struct {
		schema, name, ext, keySchema, keyBaseType, keyColumnType, colSchema, collation string
		hasEquality                                                                    bool
	}
	names, err := query.All(ctx, q, func(rows *sql.Rows) (tableNames, error) {
		var name tableNames
		err := rows.Scan(&name.schema, &name.name, &name.ext, &name.keySchema, &name.keyBaseType, &name.keyColumnType, &name.colSchema, &name.collation, &name.hasEquality)
		return name, err
	})
	if err != nil {
		return nil, fmt.Errorf("resolve chunk table: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("pgvector: table %q requires non-null text or bigint doc_key, non-null integer or bigint chunk_index, and fixed-dimension halfvec embedding", table)
	}
	name := names[0]
	var collation string
	if name.collation != "" {
		collation = " COLLATE " + postgres.QuoteIdentifier(name.colSchema) + "." + postgres.QuoteIdentifier(name.collation)
	}
	var equality string
	if name.hasEquality {
		equality = " OPERATOR(" + postgres.QuoteIdentifier(name.keySchema) + ".=) "
	}
	return &Chunks[K]{table: postgres.QuoteIdentifier(name.schema) + "." + postgres.QuoteIdentifier(name.name), ext: postgres.QuoteIdentifier(name.ext), keyBaseType: name.keyBaseType, keyColumnType: name.keyColumnType, keyCollation: collation, keyEqual: equality}, nil
}

// ReplaceTx deletes the selected documents' chunks and streams replacements.
// Keys must bind to non-NULL values and have an equality operator in their base type schema.
// Each vector must have the table's fixed dimension.
// Roll back tx on error. A nil sequence clears the selected documents.
// Callers must serialize replacements for each document through commit.
// tx must use READ COMMITTED isolation; ReplaceTx rejects other levels before
// deleting rows or reading chunks.
func (c *Chunks[K]) ReplaceTx(ctx context.Context, tx *sql.Tx, keys []K, chunks iter.Seq2[Chunk[K], error]) (int, error) {
	selected, err := selectedChunkKeys(ctx, tx, c.keyEqual, keys)
	if err != nil {
		return 0, err
	}
	var changed bool
	query := "SELECT EXISTS (SELECT 1 FROM pg_catalog.unnest($1::" + c.keyBaseType + "[]) AS key WHERE key IS NULL OR NOT (key" + c.keyCollation + c.keyEqual + "key::" + c.keyColumnType + "))"
	if err := tx.QueryRowContext(ctx, query, selected).Scan(&changed); err != nil {
		return 0, fmt.Errorf("check document keys: %w", err)
	}
	if changed {
		return 0, errors.New("pgvector: selected document key is null or changes with column precision")
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM "+c.table+" WHERE doc_key"+c.keyCollation+c.keyEqual+"ANY($1::"+c.keyBaseType+"[])", selected); err != nil {
		return 0, fmt.Errorf("delete chunks: %w", err)
	}
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM "+c.table+" WHERE doc_key"+c.keyCollation+c.keyEqual+"ANY($1::"+c.keyBaseType+"[]))", selected).Scan(&changed); err != nil {
		return 0, fmt.Errorf("check deleted chunks: %w", err)
	}
	if changed {
		return 0, errors.New("pgvector: selected chunks remain after deletion")
	}
	if chunks == nil {
		return 0, nil
	}
	selectedSet := make(map[any]struct{}, len(selected))
	for _, key := range selected {
		selectedSet[comparableChunkKey(key)] = struct{}{}
	}
	var selectedHashes map[int64][]any
	var args []any
	var batchKey any
	var values []string
	var batches []insertedBatch
	count := 0
	flush := func() error {
		if len(values) == 0 {
			return nil
		}
		ids, err := c.insertBatch(ctx, tx, batchKey, values, args)
		if err != nil {
			return err
		}
		batches = append(batches, insertedBatch{ids: ids, count: len(values)})
		args, values = nil, nil
		return nil
	}
	for chunk, err := range chunks {
		if err != nil {
			return 0, fmt.Errorf("read chunks: %w", err)
		}
		literal, err := chunkVector(chunk.Vector)
		if err != nil {
			return 0, fmt.Errorf("document %v chunk %d: %w", chunk.Doc, chunk.ChunkIndex, err)
		}
		key, err := chunkKey(chunk.Doc)
		if err != nil {
			return 0, fmt.Errorf("document key: %w", err)
		}
		if _, ok := selectedSet[comparableChunkKey(key)]; !ok {
			if selectedHashes == nil {
				selectedHashes, err = c.hashSelectedKeys(ctx, tx, selected)
				if err != nil {
					return 0, err
				}
			}
			found, err := c.hasSelectedKey(ctx, tx, key, selectedHashes)
			if err != nil {
				return 0, err
			}
			if !found {
				return 0, errors.New("pgvector: chunk document is outside the selected keys")
			}
			selectedSet[comparableChunkKey(key)] = struct{}{}
		}
		if len(values) > 0 && !reflect.DeepEqual(batchKey, key) {
			if err := flush(); err != nil {
				return 0, fmt.Errorf("insert chunks: %w", err)
			}
		}
		batchKey = key
		n := len(args) + 1
		values = append(values, fmt.Sprintf("($%d::%s,$%d::bigint,$%d::%s.halfvec)", n+1, c.keyBaseType, n+2, n+3, c.ext))
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
	if err := c.checkInsertedBatches(ctx, tx, batches); err != nil {
		return 0, fmt.Errorf("check inserted chunks: %w", err)
	}
	return count, nil
}

type insertedBatch struct {
	ids   string
	count int
}

// insertBatch checks returned keys and records row locations.
func (c *Chunks[K]) insertBatch(ctx context.Context, tx *sql.Tx, key any, values []string, args []any) (string, error) {
	query := "WITH inserted AS (INSERT INTO " + c.table + " (doc_key,chunk_index,embedding) SELECT v.doc_key,v.chunk_index,v.embedding FROM (VALUES " + strings.Join(values, ",") + ") AS v(doc_key,chunk_index,embedding) RETURNING doc_key, tableoid, ctid) SELECT count(*), count(*) FILTER (WHERE doc_key" + c.keyCollation + c.keyEqual + "$1::" + c.keyBaseType + "), array_agg(tableoid::text || ':' || ctid::text)::text FROM inserted"
	var inserted, matched int
	var ids string
	if err := tx.QueryRowContext(ctx, query, append([]any{key}, args...)...).Scan(&inserted, &matched, &ids); err != nil {
		return "", err
	}
	if matched != len(values) || inserted != len(values) {
		return "", errors.New("pgvector: chunk document is outside the selected keys")
	}
	return ids, nil
}

func (c *Chunks[K]) checkInsertedBatches(ctx context.Context, tx *sql.Tx, batches []insertedBatch) error {
	for _, batch := range batches {
		var present int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+c.table+" WHERE ctid = ANY(ARRAY(SELECT pg_catalog.split_part(id, ':', 2)::tid FROM pg_catalog.unnest($1::text[]) AS id)) AND tableoid::text || ':' || ctid::text = ANY($1::text[])", batch.ids).Scan(&present); err != nil {
			return fmt.Errorf("check inserted chunks: %w", err)
		}
		if present != batch.count {
			return errors.New("pgvector: inserted chunks changed during insertion")
		}
	}
	return nil
}

func comparableChunkKey(key any) any {
	if b, ok := key.([]byte); ok {
		return string(b)
	}
	return key
}

func (c *Chunks[K]) hashKey(expression string) string {
	if c.keyBaseType == "pg_catalog.text" {
		return "pg_catalog.hashtextextended(" + expression + c.keyCollation + ", 0)"
	}
	return "pg_catalog.hashint8extended(" + expression + ", 0)"
}

func (c *Chunks[K]) hashSelectedKeys(ctx context.Context, tx *sql.Tx, selected []any) (map[int64][]any, error) {
	query := "SELECT key, " + c.hashKey("key") + " FROM (SELECT DISTINCT key" + c.keyCollation + " AS key FROM pg_catalog.unnest($1::" + c.keyBaseType + "[]) AS key) AS selected"
	rows, err := tx.QueryContext(ctx, query, selected)
	if err != nil {
		return nil, fmt.Errorf("hash selected documents: %w", err)
	}
	defer rows.Close()
	hashes := make(map[int64][]any, len(selected))
	for rows.Next() {
		var key any
		var hash int64
		if err := rows.Scan(&key, &hash); err != nil {
			return nil, fmt.Errorf("hash selected documents: %w", err)
		}
		hashes[hash] = append(hashes[hash], key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hash selected documents: %w", err)
	}
	return hashes, nil
}

func (c *Chunks[K]) hasSelectedKey(ctx context.Context, tx *sql.Tx, key any, hashes map[int64][]any) (bool, error) {
	var hash int64
	if err := tx.QueryRowContext(ctx, "SELECT "+c.hashKey("$1::"+c.keyBaseType), key).Scan(&hash); err != nil {
		return false, fmt.Errorf("hash chunk document: %w", err)
	}
	if len(hashes[hash]) == 0 {
		return false, nil
	}
	var found bool
	query := "SELECT $1::" + c.keyBaseType + c.keyCollation + c.keyEqual + "ANY($2::" + c.keyBaseType + "[])"
	if err := tx.QueryRowContext(ctx, query, key, hashes[hash]).Scan(&found); err != nil {
		return false, fmt.Errorf("check chunk document: %w", err)
	}
	return found, nil
}

func selectedChunkKeys[K comparable](ctx context.Context, tx *sql.Tx, equality string, keys []K) ([]any, error) {
	if tx == nil {
		return nil, errors.New("pgvector: nil transaction")
	}
	if equality == "" {
		return nil, errors.New("pgvector: document key equality cannot be resolved")
	}
	if err := requireReadCommitted(ctx, tx, "ReplaceTx"); err != nil {
		return nil, err
	}
	selected := make([]any, len(keys))
	for i, key := range keys {
		value, err := chunkKey(key)
		if err != nil {
			return nil, fmt.Errorf("document key: %w", err)
		}
		selected[i] = value
	}
	return selected, nil
}

func chunkKey(key any) (driver.Value, error) {
	value, err := driver.DefaultParameterConverter.ConvertValue(key)
	if b, ok := value.([]byte); ok {
		value = bytes.Clone(b)
	}
	return value, err
}

// chunkVector formats the original components so halfvec rounds them exactly
// as an existing index does. It rejects vectors that round to zero.
func chunkVector(v vector.Vector) (string, error) {
	var nonzero bool
	for i, component := range v {
		value := float64(component)
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return "", fmt.Errorf("pgvector: vector component %d is non-finite", i)
		}
		// The smallest halfvec magnitude is 2^-24; half of it rounds to zero.
		nonzero = nonzero || math.Abs(value) > 0x1p-25
	}
	if !nonzero {
		return "", errors.New("pgvector: vector has zero norm")
	}
	return vectorliteral.Format(v), nil
}

func readHNSWSettings(ctx context.Context, tx *sql.Tx, ext, literal string) (string, string, error) {
	// Loading the type registers pgvector's transaction-local settings.
	var loaded bool
	if err := tx.QueryRowContext(ctx, "SELECT $1::"+ext+".halfvec IS NOT NULL", literal).Scan(&loaded); err != nil {
		return "", "", fmt.Errorf("load pgvector: %w", err)
	}
	var ef, iterative string
	if err := tx.QueryRowContext(ctx, `SELECT current_setting('hnsw.ef_search'), current_setting('hnsw.iterative_scan')`).Scan(&ef, &iterative); err != nil {
		return "", "", fmt.Errorf("read hnsw settings: %w", err)
	}
	return ef, iterative, nil
}

// restoreHNSW undoes SearchTx's settings. It skips PostgreSQL errors, which
// abort tx, and still restores after Go-side errors such as a failed scan.
func restoreHNSW(ctx context.Context, tx *sql.Tx, resultErr error, previousEF, previousIterative string, iterative bool) error {
	if _, ok := errors.AsType[interface {
		error
		SQLState() string
	}](resultErr); ok {
		return resultErr
	}
	// Restore settings even if the search context expired while tx remains live.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := tx.ExecContext(cleanupCtx, `SELECT set_config('hnsw.ef_search', $1, true)`, previousEF); err != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("restore hnsw.ef_search: %w", err))
	}
	if iterative {
		if _, err := tx.ExecContext(cleanupCtx, `SELECT set_config('hnsw.iterative_scan', $1, true)`, previousIterative); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("restore hnsw.iterative_scan: %w", err))
		}
	}
	return resultErr
}

// SearchTx returns raw nearest chunks. The caller owns freshness, hydration,
// exhaustion decisions, and transaction completion. Roll back tx on error.
// The query vector must have the table's fixed dimension. The caller owns tx's
// settings; SearchTx temporarily changes and restores its HNSW settings.
// Keep all other queries out of tx from the start of SearchTx until it returns.
func (c *Chunks[K]) SearchTx(ctx context.Context, tx *sql.Tx, query vector.Vector, limit int) (page ChunkPage[K], resultErr error) {
	if tx == nil {
		return page, errors.New("pgvector: nil transaction")
	}
	literal, err := chunkVector(query)
	if err != nil {
		return page, err
	}
	previousEF, previousIterative, err := readHNSWSettings(ctx, tx, c.ext, literal)
	if err != nil {
		return page, err
	}
	limit = max(limit, 0)
	efSearch := min(max(limit, 40), 1000)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", efSearch)); err != nil {
		return page, fmt.Errorf("set hnsw.ef_search: %w", err)
	}
	defer func() { resultErr = restoreHNSW(ctx, tx, resultErr, previousEF, previousIterative, true) }()
	page.ScanCap = limit
	if _, err := tx.ExecContext(ctx, `SET LOCAL hnsw.iterative_scan = 'relaxed_order'`); err != nil {
		return page, fmt.Errorf("set hnsw.iterative_scan: %w", err)
	}
	distance := fmt.Sprintf("embedding OPERATOR(%s.<=>) $1::%s.halfvec", c.ext, c.ext)
	q := sqlquery.Query{SQL: fmt.Sprintf("WITH candidates AS MATERIALIZED (SELECT doc_key,chunk_index,%s AS distance FROM %s WHERE doc_key IS NOT NULL AND chunk_index IS NOT NULL AND embedding IS NOT NULL AND %s.l2_norm(embedding) > 0 ORDER BY %s LIMIT $2) SELECT doc_key,chunk_index,1-distance FROM candidates ORDER BY distance + 0, doc_key, chunk_index", distance, c.table, c.ext, distance), Args: []any{literal, page.ScanCap}}
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
