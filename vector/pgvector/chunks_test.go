package pgvector_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/pgvector"
)

func TestChunks(t *testing.T) {
	dsn := os.Getenv("KIT_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("KIT_POSTGRES_TEST_DSN is not set")
	}
	ctx := t.Context()
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
	require.NoError(t, err)
	name := uniqueName(t, "chunks_test")
	_, err = admin.ExecContext(ctx, `CREATE SCHEMA `+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.WithoutCancel(ctx), `DROP SCHEMA `+name+` CASCADE`)
		assert.NoError(t, err)
		assert.NoError(t, admin.Close())
	})
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	params := u.Query()
	params.Set("search_path", name)
	u.RawQuery = params.Encode()
	db, err := sql.Open("pgx", u.String())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	_, err = db.ExecContext(ctx, `CREATE TABLE "Mixed chunks" (doc_key text, chunk_index integer, embedding public.halfvec(2), PRIMARY KEY(doc_key,chunk_index))`)
	require.NoError(t, err)

	// Attachment and search must use the borrowed transaction's only connection.
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	chunks, err := pgvector.AttachChunks[string](ctx, tx, "Mixed chunks")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	sequence := func(key string, n int) iter.Seq2[pgvector.Chunk[string], error] {
		return func(yield func(pgvector.Chunk[string], error) bool) {
			for i := range n {
				if !yield(pgvector.Chunk[string]{Doc: key, ChunkIndex: i, Vector: vector.Vector{2, 1}}, nil) {
					return
				}
			}
		}
	}
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	n, err := chunks.ReplaceTx(ctx, tx, []string{"replace", "covered", "other"}, func(yield func(pgvector.Chunk[string], error) bool) {
		for _, key := range []string{"replace", "covered", "other"} {
			if !yield(pgvector.Chunk[string]{Doc: key, Vector: vector.Vector{2, 1}}, nil) {
				return
			}
		}
	})
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	require.NoError(t, tx.Commit())

	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	n, err = chunks.ReplaceTx(ctx, tx, []string{"replace"}, func(yield func(pgvector.Chunk[string], error) bool) {
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

	wantErr := errors.New("stream failed")
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
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
	assert.Equal(t, 201, count)

	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = chunks.ReplaceTx(ctx, tx, []string{"replace"}, func(yield func(pgvector.Chunk[string], error) bool) {
		yield(pgvector.Chunk[string]{Doc: "replace", Vector: vector.Vector{1}}, nil)
	})
	require.Error(t, err)
	require.NoError(t, tx.Rollback())

	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	n, err = chunks.ReplaceTx(ctx, tx, []string{"replace"}, nil)
	require.NoError(t, err)
	assert.Zero(t, n)
	require.NoError(t, tx.Commit())
	_, err = db.ExecContext(ctx, `UPDATE "Mixed chunks" SET embedding='[2,1.001]' WHERE doc_key='other'`)
	require.NoError(t, err)
	tx, err = db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	hits, err := chunks.SearchTx(ctx, tx, vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, []string{"covered", "other"}, []string{hits[0].Doc, hits[1].Doc})
	assert.InDelta(t, 0.8944272, hits[0].Score, 1e-6)
	assert.InDelta(t, 0.8942525, hits[1].Score, 1e-6)
	assert.Nil(t, hits[0].Revision)
	hits, err = chunks.SearchTx(ctx, tx, vector.Vector{1, 0}, -1)
	require.NoError(t, err)
	assert.Empty(t, hits)
	require.NoError(t, tx.Commit())

	keys := make([]string, 201)
	for i := range keys {
		keys[i] = fmt.Sprintf("batch-%d", i)
	}
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = chunks.ReplaceTx(ctx, tx, keys, func(yield func(pgvector.Chunk[string], error) bool) {
		for _, key := range keys {
			if !yield(pgvector.Chunk[string]{Doc: key, Vector: vector.Vector{1, 0}}, nil) {
				return
			}
		}
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = chunks.ReplaceTx(ctx, tx, keys, nil)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM "Mixed chunks"`).Scan(&count))
	assert.Equal(t, 2, count)
	_, err = db.ExecContext(ctx, `CREATE TABLE chunks (doc_key text, chunk_index integer, embedding public.halfvec(3))`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO chunks SELECT i::text, 0, format('[%s,%s,%s]', sin(i*0.91), cos(i*1.17), sin(i*0.23))::public.halfvec FROM generate_series(1,1600) i`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE INDEX ON chunks USING hnsw (embedding public.halfvec_cosine_ops)`)
	require.NoError(t, err)
	chunks, err = pgvector.AttachChunks[string](ctx, db, "chunks")
	require.NoError(t, err)
	reader, err := sql.Open("pgx", u.String())
	require.NoError(t, err)
	defer func() { assert.NoError(t, reader.Close()) }()
	tx, err = reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `SET LOCAL enable_seqscan=off`)
	require.NoError(t, err)
	hits, err = chunks.SearchTx(ctx, tx, vector.Vector{1, 0.3, 0.1}, 1200)
	require.NoError(t, err)
	require.Equal(t, 1200, len(hits))
	for i := 1; i < len(hits); i++ {
		if hits[i-1].Score < hits[i].Score {
			t.Fatalf("hits %d and %d are out of order: %v < %v", i-1, i, hits[i-1].Score, hits[i].Score)
		}
	}
	require.NoError(t, tx.Commit())
}
