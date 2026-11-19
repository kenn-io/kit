package pgvector_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/pgvector"
)

func TestSharedChunksPrepopulatedKitSchema(t *testing.T) {
	db, _ := setup(t)
	ctx := t.Context()
	schema := pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "vectors", SharedChunks: true}
	require.NoError(t, pgvector.Install(ctx, db, schema))
	_, err := pgvector.New[int64, string](ctx, db, schema)
	require.ErrorContains(t, err, "vectors_chunks")
	_, err = db.ExecContext(ctx, `CREATE TABLE vectors_chunks (generation_id bigint NOT NULL, doc_key bigint NOT NULL REFERENCES docs(id) ON DELETE CASCADE, chunk_index bigint NOT NULL, embedding public.halfvec NOT NULL, PRIMARY KEY(generation_id,doc_key,chunk_index)); INSERT INTO docs VALUES (1,'a',1),(2,'b',1)`)
	require.NoError(t, err)
	model := vector.Generation{Model: "a", Dimensions: 2}
	gen := model.Fingerprint()
	initial, err := pgvector.New[int64, string](ctx, db, schema)
	require.NoError(t, err)
	require.NoError(t, initial.EnsureGeneration(ctx, gen, model, vector.StateActive))
	var id int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT id FROM vectors_generations WHERE gen_key=$1`, gen).Scan(&id))
	_, err = db.ExecContext(ctx, `INSERT INTO vectors_stamps VALUES ($1,1,1),($1,2,1)`, id)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO vectors_chunks VALUES ($1,1,0,'[1,0]'),($1,2,0,'[0,1]')`, id)
	require.NoError(t, err)
	store, err := pgvector.New[int64, string](ctx, db, schema)
	require.NoError(t, err)
	hits, err := store.QueryGeneration(ctx, gen, vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, int64(1), hits[0].Doc)
	assert.InDelta(t, 1, hits[0].Score, 0.001)
	assert.Equal(t, int64(2), hits[1].Doc)
	assert.InDelta(t, 0, hits[1].Score, 0.001)
}

func TestSharedChunksSaveDocumentsKeepsOtherGeneration(t *testing.T) {
	db, _ := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `CREATE TABLE vectors_chunks (generation_id bigint NOT NULL, doc_key bigint NOT NULL REFERENCES docs(id) ON DELETE CASCADE, chunk_index integer NOT NULL, embedding public.halfvec NOT NULL, PRIMARY KEY(generation_id,doc_key,chunk_index)); INSERT INTO docs VALUES (1,'a',1)`)
	require.NoError(t, err)
	schema := pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "vectors", SharedChunks: true}
	store, err := pgvector.New[int64, string](ctx, db, schema)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, "a", vector.Generation{Model: "a", Dimensions: 2}, vector.StateActive))
	require.NoError(t, store.EnsureGeneration(ctx, "b", vector.Generation{Model: "b", Dimensions: 2}, vector.StateBuilding))
	require.NoError(t, store.SaveVectors(ctx, "a", 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))
	require.NoError(t, store.SaveVectors(ctx, "b", 1, 1, []vector.ChunkVector{{Vector: vector.Vector{0, 1}}}))
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	stale, err := store.SaveDocuments(ctx, tx, "a", []pgvector.Document[int64]{{Doc: 1, Revision: 1, Vectors: []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}}})
	require.NoError(t, err)
	require.Empty(t, stale)
	require.NoError(t, tx.Commit())
	hits, err := store.QueryGeneration(ctx, "b", vector.Vector{0, 1}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(1), hits[0].Doc)
	assert.InDelta(t, 1, hits[0].Score, 0.001)
}

func TestSharedChunksDMLRole(t *testing.T) {
	db, _ := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `CREATE TABLE vectors_chunks (generation_id bigint NOT NULL, doc_key bigint NOT NULL REFERENCES docs(id) ON DELETE CASCADE, chunk_index integer NOT NULL, embedding public.halfvec NOT NULL, PRIMARY KEY(generation_id,doc_key,chunk_index)); INSERT INTO docs VALUES (1,'a',1);
 CREATE FUNCTION require_opener() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF current_setting('application_name') <> 'shared-test' THEN RAISE EXCEPTION 'opener missing'; END IF; RETURN COALESCE(NEW, OLD); END $$;
 CREATE TRIGGER generations_opener BEFORE INSERT OR UPDATE OR DELETE ON vectors_generations FOR EACH ROW EXECUTE FUNCTION require_opener();
 CREATE TRIGGER stamps_opener BEFORE INSERT OR UPDATE OR DELETE ON vectors_stamps FOR EACH ROW EXECUTE FUNCTION require_opener();
 CREATE TRIGGER chunks_opener BEFORE INSERT OR UPDATE OR DELETE ON vectors_chunks FOR EACH ROW EXECUTE FUNCTION require_opener()`)
	require.NoError(t, err)
	runtime := dmlOnly(t, db)
	calls := 0
	schema := pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "vectors", SharedChunks: true}
	schema.Begin = func(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
		calls++
		tx, err := runtime.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `SET LOCAL application_name = 'shared-test'`); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		return tx, nil
	}
	store, err := pgvector.New[int64, string](ctx, runtime, schema)
	require.NoError(t, err)
	model := vector.Generation{Dimensions: 2}
	require.NoError(t, store.EnsureGeneration(ctx, "a", model, vector.StateBuilding))
	require.NoError(t, store.SaveVectors(ctx, "a", 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))
	require.NoError(t, store.Activate(ctx, "a"))
	hits, err := store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, 3, calls)
	require.NoError(t, store.EnsureGeneration(ctx, "b", vector.Generation{Model: "b", Dimensions: 2}, vector.StateBuilding))
	require.NoError(t, store.SaveVectors(ctx, "b", 1, 1, []vector.ChunkVector{{ChunkIndex: 1, Vector: vector.Vector{0, 1}}}))
	assert.Equal(t, 5, calls)
	require.NoError(t, store.SetGenerationState(ctx, "a", vector.StateRetired))
	require.NoError(t, store.Reclaim(ctx, "a"))
	assert.Equal(t, 7, calls)
	var remaining int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM vectors_chunks`).Scan(&remaining))
	assert.Equal(t, 1, remaining)
	require.ErrorIs(t, store.EnsureGeneration(ctx, "a", model, vector.StateActive), vector.ErrRetired)
	require.ErrorIs(t, store.EnsureGeneration(ctx, "a", model, vector.StatePending), vector.ErrRetired)
	require.NoError(t, store.EnsureGeneration(ctx, "a", model, vector.StateBuilding))
	require.ErrorIs(t, store.Activate(ctx, "a"), vector.ErrUncovered)
	require.NoError(t, store.SaveVectors(ctx, "a", 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM vectors_chunks`).Scan(&count))
	assert.Equal(t, 2, count)
	hits, err = store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(1), hits[0].Doc)
	assert.Equal(t, 0, hits[0].ChunkIndex)
	assert.InDelta(t, 1, hits[0].Score, 0.001)
	hits, err = store.QueryGeneration(ctx, "b", vector.Vector{0, 1}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(1), hits[0].Doc)
	assert.Equal(t, 1, hits[0].ChunkIndex)
	assert.InDelta(t, 1, hits[0].Score, 0.001)
	sentinel := errors.New("begin failed")
	schema.Begin = func(context.Context, *sql.TxOptions) (*sql.Tx, error) { return nil, sentinel }
	failing, err := pgvector.New[int64, string](ctx, runtime, schema)
	require.NoError(t, err)
	for _, write := range []func() error{
		func() error { return failing.EnsureGeneration(ctx, "a", model, vector.StateBuilding) },
		func() error { return failing.SaveVectors(ctx, "a", 1, 1, nil) },
		func() error { return failing.SetGenerationState(ctx, "a", vector.StateRetired) },
		func() error { return failing.Activate(ctx, "a") },
		func() error { return failing.Reclaim(ctx, "a") },
	} {
		require.ErrorIs(t, write(), sentinel)
	}
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM vectors_chunks`).Scan(&count))
	assert.Equal(t, 2, count)
	var state string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT state FROM vectors_generations WHERE gen_key='a'`).Scan(&state))
	assert.Equal(t, "building", state)
}
