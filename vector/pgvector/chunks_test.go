package pgvector_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"math"
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/pgvector"
)

func TestChunks(t *testing.T) {
	setupChunkDB := func(t *testing.T) *sql.DB {
		t.Helper()
		dsn := os.Getenv("KIT_POSTGRES_TEST_DSN")
		if dsn == "" {
			t.Skip("KIT_POSTGRES_TEST_DSN is not set")
		}
		admin, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		_, err = admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector`)
		require.NoError(t, err)
		name := uniqueName(t, "chunks_test")
		_, err = admin.ExecContext(t.Context(), `CREATE SCHEMA `+name)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := admin.ExecContext(context.WithoutCancel(t.Context()), `DROP SCHEMA `+name+` CASCADE`)
			assert.NoError(t, err)
			assert.NoError(t, admin.Close())
		})
		u, err := url.Parse(dsn)
		require.NoError(t, err)
		q := u.Query()
		q.Set("search_path", name)
		u.RawQuery = q.Encode()
		db, err := sql.Open("pgx", u.String())
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, db.Close()) })
		return db
	}
	setupChunks := func(t *testing.T) (*sql.DB, *pgvector.Chunks[string]) {
		t.Helper()
		db := setupChunkDB(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `CREATE TABLE "Mixed chunks" (doc_key text, chunk_index integer, embedding public.halfvec(2), PRIMARY KEY(doc_key,chunk_index))`)
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err)
		chunks, err := pgvector.AttachChunks[string](ctx, tx, "Mixed chunks", 2)
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
		n, err := chunks.ReplaceTx(ctx, tx, []string{"replace"}, func(yield func(pgvector.Chunk[string], error) bool) {
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
		keys := make([]string, 201)
		keys[0] = "replace"
		for i := 1; i < len(keys)-1; i++ {
			keys[i] = fmt.Sprintf("batch-%d", i)
		}
		keys[200] = "covered"
		tx, err = db.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = chunks.ReplaceTx(ctx, tx, keys, nil)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		var count int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM "Mixed chunks"`).Scan(&count))
		assert.Equal(t, 1, count)
		var remaining string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT doc_key FROM "Mixed chunks"`).Scan(&remaining))
		assert.Equal(t, "other", remaining)
	})

	t.Run("search", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `INSERT INTO "Mixed chunks" VALUES ('covered', 0, '[2,1.001]'), ('other', 0, '[2,1]')`)
		require.NoError(t, err)
		db.SetMaxIdleConns(0)
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
		db := setupChunkDB(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `CREATE TABLE chunks (doc_key text, chunk_index integer, embedding public.halfvec(3))`)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO chunks SELECT i::text, 0, format('[%s,%s,%s]', sin(i*0.91), cos(i*1.17), sin(i*0.23))::public.halfvec FROM generate_series(1,1600) i`)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `CREATE INDEX chunks_hnsw_idx ON chunks USING hnsw (embedding public.halfvec_cosine_ops)`)
		require.NoError(t, err)
		chunks, err := pgvector.AttachChunks[string](ctx, db, "chunks", 3)
		require.NoError(t, err)
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
		for i := 1; i < len(page.Hits); i++ {
			assert.GreaterOrEqual(t, page.Hits[i-1].Score, page.Hits[i].Score)
		}
		assert.Equal(t, 1200, page.ScanCap)
		var ef, iterative string
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT current_setting('hnsw.ef_search'), current_setting('hnsw.iterative_scan')`).Scan(&ef, &iterative))
		assert.Equal(t, "77", ef)
		assert.Equal(t, "strict_order", iterative)
		require.NoError(t, tx.Commit())
		var scans int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT idx_scan FROM pg_stat_user_indexes WHERE indexrelid='chunks_hnsw_idx'::regclass`).Scan(&scans))
		assert.Positive(t, scans)
	})

	t.Run("invalid vectors and attachment", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		_, err := pgvector.AttachChunks[string](ctx, db, "Mixed chunks", 3)
		require.ErrorContains(t, err, "no extension-owned halfvec embedding")
		seed(t, db, chunks, "replace")
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = chunks.SearchTx(ctx, tx, vector.Vector{1}, 1)
		require.ErrorContains(t, err, "vector dimension")
		_, err = chunks.ReplaceTx(ctx, tx, []string{"replace"}, func(yield func(pgvector.Chunk[string], error) bool) {
			yield(pgvector.Chunk[string]{Doc: "replace", Vector: vector.Vector{1}}, nil)
		})
		require.ErrorContains(t, err, "vector dimension")
		_, err = chunks.ReplaceTx(ctx, tx, []string{"invalid-doc"}, func(yield func(pgvector.Chunk[string], error) bool) {
			yield(pgvector.Chunk[string]{Doc: "invalid-doc", ChunkIndex: 7, Vector: vector.Vector{float32(math.Inf(1)), 0}}, nil)
		})
		require.ErrorContains(t, err, "non-finite")
		_, err = chunks.ReplaceTx(ctx, tx, []string{"selected"}, func(yield func(pgvector.Chunk[string], error) bool) {
			yield(pgvector.Chunk[string]{Doc: "outside", Vector: vector.Vector{1, 0}}, nil)
		})
		require.ErrorContains(t, err, "outside")
		wantErr := errors.New("stream failed")
		_, err = chunks.ReplaceTx(ctx, tx, []string{"replace"}, func(yield func(pgvector.Chunk[string], error) bool) {
			for chunk, err := range sequence("replace", 201) {
				if !yield(chunk, err) {
					return
				}
			}
			yield(pgvector.Chunk[string]{}, wantErr)
		})
		require.ErrorIs(t, err, wantErr)
		require.NoError(t, tx.Rollback())
		var count int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM "Mixed chunks" WHERE doc_key='replace'`).Scan(&count))
		assert.Equal(t, 1, count)
	})
	t.Run("nullable embeddings", func(t *testing.T) {
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
	})
	t.Run("zero vectors", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = chunks.SearchTx(ctx, tx, vector.Vector{1e-20, 0}, 1)
		require.ErrorContains(t, err, "zero")
		_, err = chunks.ReplaceTx(ctx, tx, []string{"zero"}, func(yield func(pgvector.Chunk[string], error) bool) {
			yield(pgvector.Chunk[string]{Doc: "zero", Vector: vector.Vector{0, 0}}, nil)
		})
		require.ErrorContains(t, err, "zero")
	})
	t.Run("binary and UUID keys", func(t *testing.T) {
		db := setupChunkDB(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `CREATE TABLE binary_chunks (doc_key bytea, chunk_index int, embedding public.halfvec(2)); CREATE TABLE uuid_chunks (doc_key uuid, chunk_index int, embedding public.halfvec(2))`)
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		uuids, err := pgvector.AttachChunks[string](ctx, tx, "uuid_chunks", 2)
		require.NoError(t, err)
		_, err = uuids.ReplaceTx(ctx, tx, []string{"ABCDEFAB-CDEF-ABCD-EFAB-CDEFABCDEFAB"}, func(yield func(pgvector.Chunk[string], error) bool) {
			yield(pgvector.Chunk[string]{Doc: "abcdefab-cdef-abcd-efab-cdefabcdefab", Vector: vector.Vector{1, 0}}, nil)
		})
		require.NoError(t, err)
		binary, err := pgvector.AttachChunks[any](ctx, tx, "binary_chunks", 2)
		require.NoError(t, err)
		_, err = binary.ReplaceTx(ctx, tx, []any{[]byte("key")}, func(yield func(pgvector.Chunk[any], error) bool) {
			yield(pgvector.Chunk[any]{Doc: []byte("key"), Vector: vector.Vector{1, 0}}, nil)
		})
		require.NoError(t, err)
		_, err = binary.ReplaceTx(ctx, tx, []any{[]int{1}}, nil)
		require.ErrorContains(t, err, "unsupported document key")
	})
}
