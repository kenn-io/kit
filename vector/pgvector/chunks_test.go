package pgvector_test

import (
	"database/sql"
	"errors"
	"iter"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/pgvector"
)

func TestChunks(t *testing.T) {
	setupChunks := func(t *testing.T) (*sql.DB, *pgvector.Chunks[string]) {
		t.Helper()
		db := setupDB(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `CREATE TABLE "Mixed chunks" (doc_key text, chunk_index integer, embedding public.halfvec(2), PRIMARY KEY(doc_key,chunk_index))`)
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err)
		chunks, err := pgvector.AttachChunks[string](ctx, tx, "Mixed chunks")
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		return db, chunks
	}
	sequence := func(key string, n int) iter.Seq2[pgvector.Chunk[string], error] {
		return func(yield func(pgvector.Chunk[string], error) bool) {
			for i := range n {
				if !yield(pgvector.Chunk[string]{Doc: key, ChunkIndex: i, Vector: vector.Vector{2, 1}}, nil) {
					return
				}
			}
		}
	}
	seed := func(t *testing.T, db *sql.DB, chunks *pgvector.Chunks[string], keys ...string) {
		t.Helper()
		ctx := t.Context()
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		n, err := chunks.ReplaceTx(ctx, tx, keys, func(yield func(pgvector.Chunk[string], error) bool) {
			for _, key := range keys {
				if !yield(pgvector.Chunk[string]{Doc: key, Vector: vector.Vector{2, 1}}, nil) {
					return
				}
			}
		})
		require.NoError(t, err)
		assert.Equal(t, len(keys), n)
		require.NoError(t, tx.Commit())
	}

	t.Run("replace batching", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		seed(t, db, chunks, "replace", "covered", "other")
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		n, err := chunks.ReplaceTx(ctx, tx, []string{"replace", "covered"}, func(yield func(pgvector.Chunk[string], error) bool) {
			for chunk, err := range sequence("replace", 201) {
				if chunk.ChunkIndex == 200 {
					var flushed int
					require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM "Mixed chunks" WHERE doc_key='replace'`).Scan(&flushed))
					assert.Equal(t, 200, flushed)
				}
				if !yield(chunk, err) {
					return
				}
			}
		})
		require.NoError(t, err)
		assert.Equal(t, 201, n)
		require.NoError(t, tx.Commit())
		var stored string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT embedding::text FROM "Mixed chunks" WHERE doc_key='replace' AND chunk_index=200`).Scan(&stored))
		assert.Equal(t, "[2,1]", stored)
		var count int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM "Mixed chunks" WHERE doc_key != 'replace'`).Scan(&count))
		assert.Equal(t, 1, count)
		var remaining string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT doc_key FROM "Mixed chunks" WHERE doc_key != 'replace'`).Scan(&remaining))
		assert.Equal(t, "other", remaining)
	})

	t.Run("search", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `INSERT INTO "Mixed chunks" VALUES ('covered', 0, '[2,1.001]'), ('other', 0, '[2,1]')`)
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err)
		page, err := chunks.SearchTx(ctx, tx, vector.Vector{1, 0}, 60)
		require.NoError(t, err)
		require.Len(t, page.Hits, 2)
		assert.Equal(t, []string{"other", "covered"}, []string{page.Hits[0].Doc, page.Hits[1].Doc})
		assert.InDelta(t, 0.8944272, page.Hits[0].Score, 1e-6)
		assert.InDelta(t, 0.8942525, page.Hits[1].Score, 1e-6)
		assert.Nil(t, page.Hits[0].Revision)
		assert.Equal(t, 60, page.ScanCap)
		var efSearch string
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT current_setting('hnsw.ef_search')`).Scan(&efSearch))
		assert.Equal(t, "40", efSearch)
		page, err = chunks.SearchTx(ctx, tx, vector.Vector{1, 0}, -1)
		require.NoError(t, err)
		assert.Empty(t, page.Hits)
		assert.Zero(t, page.ScanCap)
		require.NoError(t, tx.Rollback())
	})

	t.Run("large limit search", func(t *testing.T) {
		db := setupDB(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `CREATE TABLE chunks (doc_key text, chunk_index integer, embedding public.halfvec(3))`)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO chunks SELECT i::text, 0, format('[%s,%s,%s]', sin(i*0.91), cos(i*1.17), sin(i*0.23))::public.halfvec FROM generate_series(1,1600) i`)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `CREATE INDEX chunks_hnsw_idx ON chunks USING hnsw (embedding public.halfvec_cosine_ops)`)
		require.NoError(t, err)
		chunks, err := pgvector.AttachChunks[string](ctx, db, "chunks")
		require.NoError(t, err)
		// Search on a fresh connection where pgvector has not yet been loaded.
		db.SetMaxIdleConns(0)
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, `SET LOCAL enable_seqscan=off`)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `SET LOCAL hnsw.ef_search=77; SET LOCAL hnsw.iterative_scan='strict_order'`)
		require.NoError(t, err)
		page, err := chunks.SearchTx(ctx, tx, vector.Vector{1, 0.3, 0.1}, 1200)
		require.NoError(t, err)
		require.Len(t, page.Hits, 1200)
		assert.Equal(t, 1200, page.ScanCap)
		for i := 1; i < len(page.Hits); i++ {
			assert.GreaterOrEqual(t, page.Hits[i-1].Score, page.Hits[i].Score)
		}
		var ef, iterative string
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT current_setting('hnsw.ef_search'), current_setting('hnsw.iterative_scan')`).Scan(&ef, &iterative))
		assert.Equal(t, "77", ef)
		assert.Equal(t, "strict_order", iterative)
		require.NoError(t, tx.Commit())
		_, err = db.ExecContext(ctx, `SELECT pg_stat_force_next_flush(); SELECT pg_stat_clear_snapshot()`)
		require.NoError(t, err)
		var scans int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT idx_scan FROM pg_stat_user_indexes WHERE indexrelid='chunks_hnsw_idx'::regclass`).Scan(&scans))
		assert.Positive(t, scans)
	})

	t.Run("stream error", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		wantErr := errors.New("stream failed")
		_, err = chunks.ReplaceTx(ctx, tx, []string{"replace"}, func(yield func(pgvector.Chunk[string], error) bool) {
			if !yield(pgvector.Chunk[string]{Doc: "replace", Vector: vector.Vector{2, 1}}, nil) {
				return
			}
			yield(pgvector.Chunk[string]{}, wantErr)
		})
		require.ErrorIs(t, err, wantErr)
		require.NoError(t, tx.Rollback())
	})
	t.Run("attachment follows resolved table", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		seed(t, db, chunks, "original")
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, `CREATE TEMP TABLE "Mixed chunks" (doc_key text, chunk_index int, embedding public.halfvec(2)); INSERT INTO "Mixed chunks" VALUES ('shadow',0,'[1,0]'); SET LOCAL search_path=pg_temp`)
		require.NoError(t, err)
		page, err := chunks.SearchTx(ctx, tx, vector.Vector{1, 0}, 10)
		require.NoError(t, err)
		require.Len(t, page.Hits, 1)
		assert.Equal(t, "original", page.Hits[0].Doc)
		_, err = chunks.ReplaceTx(ctx, tx, []string{"original"}, sequence("original", 1))
		require.NoError(t, err)
		var shadow string
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT doc_key FROM "Mixed chunks"`).Scan(&shadow))
		assert.Equal(t, "shadow", shadow)
		page, err = chunks.SearchTx(ctx, tx, vector.Vector{1, 0}, 10)
		require.NoError(t, err)
		require.Len(t, page.Hits, 1)
		assert.Equal(t, "original", page.Hits[0].Doc)
	})

	t.Run("clear and reject unselected keys", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		seed(t, db, chunks, "selected", "other")
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		n, err := chunks.ReplaceTx(ctx, tx, []string{"selected"}, nil)
		require.NoError(t, err)
		assert.Zero(t, n)
		require.NoError(t, tx.Commit())
		tx, err = db.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = chunks.ReplaceTx(ctx, tx, []string{"selected"}, sequence("other", 1))
		require.ErrorContains(t, err, "outside the selected keys")
		require.NoError(t, tx.Rollback())
		var count int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM "Mixed chunks"`).Scan(&count))
		assert.Equal(t, 1, count)
	})

	t.Run("invalid and nullable vectors", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `INSERT INTO "Mixed chunks" VALUES ('null',0,NULL), ('zero',0,'[0,0]'), ('valid',0,'[2,1]')`)
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		page, err := chunks.SearchTx(ctx, tx, vector.Vector{1, 0}, 10)
		require.NoError(t, err)
		require.Len(t, page.Hits, 1)
		assert.Equal(t, "valid", page.Hits[0].Doc)
		_, err = chunks.SearchTx(ctx, tx, vector.Vector{1e-20, 0}, 1)
		require.ErrorContains(t, err, "zero")
		_, err = chunks.ReplaceTx(ctx, tx, []string{"valid"}, func(yield func(pgvector.Chunk[string], error) bool) {
			yield(pgvector.Chunk[string]{Doc: "valid", Vector: vector.Vector{0, 0}}, nil)
		})
		require.ErrorContains(t, err, "zero")
		_, err = chunks.SearchTx(ctx, tx, vector.Vector{float32(math.Inf(1)), 0}, 1)
		require.ErrorContains(t, err, "non-finite")
	})

	t.Run("database key equality and attachment columns", func(t *testing.T) {
		db := setupDB(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `CREATE TABLE uuid_chunks (doc_key uuid, chunk_index int, embedding public.halfvec(2)); CREATE TABLE binary_chunks (doc_key bytea, chunk_index bigint, embedding public.halfvec(2)); CREATE TABLE missing_position (doc_key text, embedding public.halfvec(2)); CREATE TABLE missing_key (chunk_index int, embedding public.halfvec(2))`)
		require.NoError(t, err)
		_, err = pgvector.AttachChunks[string](ctx, db, "missing_position")
		require.Error(t, err)
		_, err = pgvector.AttachChunks[string](ctx, db, "missing_key")
		require.Error(t, err)
		chunks, err := pgvector.AttachChunks[string](ctx, db, "uuid_chunks")
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		n, err := chunks.ReplaceTx(ctx, tx, []string{"ABCDEFAB-CDEF-ABCD-EFAB-CDEFABCDEFAB"}, func(yield func(pgvector.Chunk[string], error) bool) {
			yield(pgvector.Chunk[string]{Doc: "abcdefab-cdef-abcd-efab-cdefabcdefab", Vector: vector.Vector{1, 0}}, nil)
		})
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		binary, err := pgvector.AttachChunks[any](ctx, tx, "binary_chunks")
		require.NoError(t, err)
		_, err = binary.ReplaceTx(ctx, tx, []any{[]byte("key")}, func(yield func(pgvector.Chunk[any], error) bool) {
			yield(pgvector.Chunk[any]{Doc: []byte("key"), Vector: vector.Vector{1, 0}}, nil)
		})
		require.NoError(t, err)
	})

	t.Run("collated key equality", func(t *testing.T) {
		db := setupDB(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `CREATE COLLATION case_insensitive (provider = icu, locale = 'und-u-ks-level2', deterministic = false); CREATE TABLE collated_chunks (doc_key text COLLATE case_insensitive, chunk_index int, embedding public.halfvec(2))`)
		require.NoError(t, err)
		chunks, err := pgvector.AttachChunks[string](ctx, db, "collated_chunks")
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		n, err := chunks.ReplaceTx(ctx, tx, []string{"KEY"}, sequence("key", 1))
		require.NoError(t, err)
		assert.Equal(t, 1, n)
	})
}
