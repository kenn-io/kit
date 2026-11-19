package pgvector_test

import (
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/pgvector"
)

func TestChunks(t *testing.T) {
	setupChunks := func(t *testing.T) (*sql.DB, *pgvector.Chunks[string]) {
		t.Helper()
		db, _ := setup(t)
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
		assert.Equal(t, "[1,0.5]", stored)
		keys := make([]string, 201)
		keys[0] = "replace"
		for i := 1; i < len(keys); i++ {
			keys[i] = fmt.Sprintf("batch-%d", i)
		}
		tx, err = db.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = chunks.ReplaceTx(ctx, tx, keys, nil)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		var count int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM "Mixed chunks"`).Scan(&count))
		assert.Equal(t, 2, count)
	})

	t.Run("stream error", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		wantErr := errors.New("stream failed")
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = chunks.ReplaceTx(ctx, tx, []string{"replace"}, func(yield func(pgvector.Chunk[string], error) bool) {
			if !yield(pgvector.Chunk[string]{Doc: "replace", Vector: vector.Vector{2, 1}}, nil) {
				return
			}
			yield(pgvector.Chunk[string]{}, wantErr)
		})
		require.ErrorIs(t, err, wantErr)
		require.NoError(t, tx.Rollback())
	})

	t.Run("wrong dimension", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = chunks.ReplaceTx(ctx, tx, []string{"replace"}, func(yield func(pgvector.Chunk[string], error) bool) {
			yield(pgvector.Chunk[string]{Doc: "replace", Vector: vector.Vector{1}}, nil)
		})
		require.ErrorContains(t, err, "insert chunks")
		require.NoError(t, tx.Rollback())
	})

	t.Run("search", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		seed(t, db, chunks, "covered", "other")
		_, err := db.ExecContext(ctx, `UPDATE "Mixed chunks" SET embedding='[2,1.001]' WHERE doc_key='other'`)
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err)
		hits, err := chunks.SearchTx(ctx, tx, vector.Vector{1, 0}, 10)
		require.NoError(t, err)
		require.Len(t, hits, 2)
		assert.Equal(t, []string{"covered", "other"}, []string{hits[0].Doc, hits[1].Doc})
		assert.InDelta(t, 0.8944272, hits[0].Score, 1e-6)
		assert.InDelta(t, 0.8942525, hits[1].Score, 1e-6)
		_, err = chunks.SearchTx(ctx, tx, vector.Vector{1, 0}, -1)
		require.ErrorContains(t, err, "search chunks")
		require.NoError(t, tx.Rollback())
	})

	t.Run("large limit search", func(t *testing.T) {
		db, _ := setup(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `CREATE TABLE chunks (doc_key text, chunk_index integer, embedding public.halfvec(3))`)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO chunks SELECT i::text, 0, format('[%s,%s,%s]', sin(i*0.91), cos(i*1.17), sin(i*0.23))::public.halfvec FROM generate_series(1,1600) i`)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `CREATE INDEX ON chunks USING hnsw (embedding public.halfvec_cosine_ops)`)
		require.NoError(t, err)
		chunks, err := pgvector.AttachChunks[string](ctx, db, "chunks")
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, `SET LOCAL enable_seqscan=off`)
		require.NoError(t, err)
		hits, err := chunks.SearchTx(ctx, tx, vector.Vector{1, 0.3, 0.1}, 1200)
		require.NoError(t, err)
		require.Equal(t, 1200, len(hits))
		for i := 1; i < len(hits); i++ {
			assert.GreaterOrEqual(t, hits[i-1].Score, hits[i].Score, "hits %d and %d", i-1, i)
		}
		require.NoError(t, tx.Commit())
	})

	t.Run("tiny query vector", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = chunks.ReplaceTx(ctx, tx, []string{"aligned", "orthogonal"}, func(yield func(pgvector.Chunk[string], error) bool) {
			if !yield(pgvector.Chunk[string]{Doc: "aligned", Vector: vector.Vector{1, 0}}, nil) {
				return
			}
			yield(pgvector.Chunk[string]{Doc: "orthogonal", Vector: vector.Vector{0, 1}}, nil)
		})
		require.NoError(t, err)
		hits, err := chunks.SearchTx(ctx, tx, vector.Vector{1e-20, 0}, 2)
		require.NoError(t, err)
		require.Len(t, hits, 2)
		assert.Equal(t, "aligned", hits[0].Doc)
		assert.InDelta(t, 1, hits[0].Score, 0.001)
		assert.Equal(t, "orthogonal", hits[1].Doc)
	})

	t.Run("invalid zero", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = chunks.ReplaceTx(ctx, tx, []string{"invalid-doc"}, func(yield func(pgvector.Chunk[string], error) bool) {
			yield(pgvector.Chunk[string]{Doc: "invalid-doc", ChunkIndex: 7, Vector: vector.Vector{0, 0}}, nil)
		})
		require.Error(t, err)
		assert.ErrorContains(t, err, "invalid-doc")
		assert.ErrorContains(t, err, "chunk 7")
	})

	t.Run("streamed document outside keys", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = chunks.ReplaceTx(ctx, tx, []string{"selected"}, func(yield func(pgvector.Chunk[string], error) bool) {
			yield(pgvector.Chunk[string]{Doc: "outside", Vector: vector.Vector{1, 0}}, nil)
		})
		require.Error(t, err)
		assert.ErrorContains(t, err, "outside")
	})
}
