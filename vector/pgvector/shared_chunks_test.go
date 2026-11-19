package pgvector_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/sqlquery"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/pgvector"
)

func TestSharedChunks(t *testing.T) {
	db, _ := setup(t)
	ctx := t.Context()
	db.SetMaxOpenConns(1)
	schema := pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "vectors", SharedChunks: true}
	_, err := pgvector.New[int64, string](ctx, db, schema)
	require.Error(t, err)
	_, err = db.ExecContext(ctx, `CREATE TABLE vectors_chunks (generation_id bigint, doc_key bigint, chunk_index integer, embedding text)`)
	require.NoError(t, err)
	_, err = pgvector.New[int64, string](ctx, db, schema)
	require.Error(t, err)
	_, err = db.ExecContext(ctx, `DROP TABLE vectors_chunks; CREATE TABLE vectors_chunks (generation_id bigint, doc_key bigint, chunk_index integer, embedding public.halfvec(2))`)
	require.NoError(t, err)
	db.SetMaxIdleConns(0)
	db.SetMaxIdleConns(2)
	_, err = pgvector.New[int64, string](ctx, db, schema)
	require.Error(t, err)
	_, err = db.ExecContext(ctx, `DROP TABLE vectors_chunks; CREATE TABLE vectors_chunks (generation_id bigint NOT NULL, doc_key bigint NOT NULL REFERENCES docs(id) ON DELETE CASCADE, chunk_index integer NOT NULL, embedding public.halfvec NOT NULL, PRIMARY KEY (generation_id,doc_key,chunk_index)); INSERT INTO docs VALUES (1,'a',1),(2,'b',1),(3,'c',1)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE FUNCTION require_opener() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF current_setting('application_name') <> 'shared-test' THEN RAISE EXCEPTION 'opener missing'; END IF; RETURN COALESCE(NEW, OLD); END $$;
 CREATE TRIGGER generations_opener BEFORE INSERT OR UPDATE OR DELETE ON vectors_generations FOR EACH ROW EXECUTE FUNCTION require_opener();
 CREATE TRIGGER stamps_opener BEFORE INSERT OR UPDATE OR DELETE ON vectors_stamps FOR EACH ROW EXECUTE FUNCTION require_opener();
 CREATE TRIGGER chunks_opener BEFORE INSERT OR UPDATE OR DELETE ON vectors_chunks FOR EACH ROW EXECUTE FUNCTION require_opener()`)
	require.NoError(t, err)
	calls := 0
	schema.Begin = func(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
		calls++
		tx, err := db.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `SET LOCAL application_name = 'shared-test'`); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		return tx, nil
	}
	db.SetMaxIdleConns(0)
	db.SetMaxIdleConns(2)
	store, err := pgvector.New[int64, string](ctx, db, schema)
	require.NoError(t, err)
	model := vector.Generation{Model: "a", Dimensions: 2}
	require.NoError(t, store.EnsureGeneration(ctx, "a", model, vector.StateBuilding))
	for id, v := range map[int64]vector.Vector{1: {1, 0}, 2: {0, 1}, 3: {1, 1}} {
		require.NoError(t, store.SaveVectors(ctx, "a", id, 1, []vector.ChunkVector{{Vector: v}}))
	}
	require.NoError(t, store.Activate(ctx, "a"))
	require.NoError(t, store.EnsureGeneration(ctx, "b", vector.Generation{Model: "b", Dimensions: 3}, vector.StateBuilding))
	require.NoError(t, store.SaveVectors(ctx, "b", 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0, 0}}}))
	hits, err := store.QueryGeneration(ctx, "b", vector.Vector{1, 0, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(1), hits[0].Doc)
	_, err = db.ExecContext(ctx, `UPDATE docs SET revision=2 WHERE id=1`)
	require.NoError(t, err)
	hits, err = store.QueryGenerationWithPredicate(ctx, "a", vector.Vector{1, 0}, 1, sqlquery.Predicate{SQL: "d.id = ?", Args: []any{int64(2)}})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(2), hits[0].Doc)
	hits, err = store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, int64(3), hits[0].Doc)
	require.NoError(t, store.SetGenerationState(ctx, "a", vector.StateRetired))
	require.NoError(t, store.Reclaim(ctx, "a"))
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM vectors_chunks`).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, store.EnsureGeneration(ctx, "a", model, vector.StateBuilding))
	require.NoError(t, store.SaveVectors(ctx, "a", 1, 2, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))
	hits, err = store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(1), hits[0].Doc)
	assert.Equal(t, 11, calls)
	sentinel := errors.New("begin failed")
	schema.Begin = func(context.Context, *sql.TxOptions) (*sql.Tx, error) { return nil, sentinel }
	failing, err := pgvector.New[int64, string](ctx, db, schema)
	require.NoError(t, err)
	for _, write := range []func() error{
		func() error { return failing.EnsureGeneration(ctx, "a", model, vector.StateBuilding) },
		func() error { return failing.SaveVectors(ctx, "a", 1, 2, nil) },
		func() error { return failing.SetGenerationState(ctx, "a", vector.StateRetired) },
		func() error { return failing.Activate(ctx, "a") },
		func() error { return failing.Reclaim(ctx, "a") },
	} {
		require.ErrorIs(t, write(), sentinel)
	}
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM vectors_chunks`).Scan(&count))
	assert.Equal(t, 2, count)
}

func TestSharedChunksDMLRole(t *testing.T) {
	db, _ := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `CREATE TABLE vectors_chunks (generation_id bigint NOT NULL, doc_key bigint NOT NULL REFERENCES docs(id) ON DELETE CASCADE, chunk_index integer NOT NULL, embedding public.halfvec NOT NULL, PRIMARY KEY(generation_id,doc_key,chunk_index)); INSERT INTO docs VALUES (1,'a',1)`)
	require.NoError(t, err)
	role := uniqueName(t, "vec_runtime")
	_, err = db.ExecContext(ctx, `CREATE ROLE `+role+` LOGIN PASSWORD 'test'`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.ExecContext(context.WithoutCancel(t.Context()), `DROP OWNED BY `+role+`; DROP ROLE `+role)
		assert.NoError(t, err)
	})
	var name string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&name))
	_, err = db.ExecContext(ctx, `GRANT USAGE ON SCHEMA `+name+` TO `+role+`; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA `+name+` TO `+role+`; GRANT USAGE ON ALL SEQUENCES IN SCHEMA `+name+` TO `+role)
	require.NoError(t, err)
	u, err := url.Parse(os.Getenv("KIT_POSTGRES_TEST_DSN"))
	require.NoError(t, err)
	u.User = url.UserPassword(role, "test")
	q := u.Query()
	q.Set("search_path", name)
	u.RawQuery = q.Encode()
	runtime, err := sql.Open("pgx", u.String())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })
	store, err := pgvector.New[int64, string](ctx, runtime, pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "vectors", SharedChunks: true})
	require.NoError(t, err)
	model := vector.Generation{Dimensions: 2}
	require.NoError(t, store.EnsureGeneration(ctx, "a", model, vector.StateBuilding))
	require.NoError(t, store.SaveVectors(ctx, "a", 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))
	require.NoError(t, store.Activate(ctx, "a"))
	hits, err := store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.NoError(t, store.SetGenerationState(ctx, "a", vector.StateRetired))
	require.NoError(t, store.Reclaim(ctx, "a"))
	require.NoError(t, store.EnsureGeneration(ctx, "a", model, vector.StateBuilding))
	require.NoError(t, store.SaveVectors(ctx, "a", 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))
	hits, err = store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
}

func TestSharedChunksMatchesDefaultRanking(t *testing.T) {
	db, normal := setup(t)
	ctx := t.Context()
	schema := pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "shared", SharedChunks: true}
	require.NoError(t, pgvector.Install(ctx, db, schema))
	_, err := db.ExecContext(ctx, `CREATE TABLE shared_chunks (generation_id bigint NOT NULL, doc_key bigint NOT NULL REFERENCES docs(id) ON DELETE CASCADE, chunk_index integer NOT NULL, embedding public.halfvec NOT NULL, PRIMARY KEY(generation_id,doc_key,chunk_index)); INSERT INTO docs VALUES (1,'a',1),(2,'b',1),(3,'c',1)`)
	require.NoError(t, err)
	shared, err := pgvector.New[int64, string](ctx, db, schema)
	require.NoError(t, err)
	for _, store := range []*pgvector.Store[int64, string]{normal, shared} {
		require.NoError(t, store.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateBuilding))
		for id, v := range map[int64]vector.Vector{1: {1, 0}, 2: {0, 1}, 3: {1, 1}} {
			require.NoError(t, store.SaveVectors(ctx, "a", id, 1, []vector.ChunkVector{{Vector: v}}))
		}
	}
	expected, err := normal.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 3)
	require.NoError(t, err)
	actual, err := shared.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 3)
	require.NoError(t, err)
	require.Equal(t, expected, actual)
}

func TestSharedChunksReclaimedGenerationCannotEnsureActive(t *testing.T) {
	db, _ := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `CREATE TABLE vectors_chunks (generation_id bigint NOT NULL, doc_key bigint NOT NULL REFERENCES docs(id) ON DELETE CASCADE, chunk_index integer NOT NULL, embedding public.halfvec NOT NULL, PRIMARY KEY(generation_id,doc_key,chunk_index)); INSERT INTO docs VALUES (1,'a',1)`)
	require.NoError(t, err)
	store, err := pgvector.New[int64, string](ctx, db, pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "vectors", SharedChunks: true})
	require.NoError(t, err)
	model := vector.Generation{Dimensions: 2}
	require.NoError(t, store.EnsureGeneration(ctx, "a", model, vector.StateBuilding))
	require.NoError(t, store.SaveVectors(ctx, "a", 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))
	require.NoError(t, store.Activate(ctx, "a"))
	require.NoError(t, store.SetGenerationState(ctx, "a", vector.StateRetired))
	require.NoError(t, store.Reclaim(ctx, "a"))
	require.ErrorIs(t, store.EnsureGeneration(ctx, "a", model, vector.StateActive), vector.ErrRetired)
	require.ErrorIs(t, store.EnsureGeneration(ctx, "a", model, vector.StatePending), vector.ErrRetired)
	require.NoError(t, store.EnsureGeneration(ctx, "a", model, vector.StateBuilding))
	require.ErrorIs(t, store.Activate(ctx, "a"), vector.ErrUncovered)
}

func TestSharedChunksRequiresDocumentCascade(t *testing.T) {
	db, _ := setup(t)
	ctx := t.Context()
	schema := pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "vectors", SharedChunks: true}
	_, err := db.ExecContext(ctx, `CREATE TABLE vectors_chunks (generation_id bigint NOT NULL, doc_key bigint NOT NULL, chunk_index integer NOT NULL, embedding public.halfvec NOT NULL, PRIMARY KEY(generation_id,doc_key,chunk_index))`)
	require.NoError(t, err)
	_, err = pgvector.New[int64, string](ctx, db, schema)
	require.Error(t, err)
	_, err = db.ExecContext(ctx, `ALTER TABLE vectors_chunks ADD FOREIGN KEY (doc_key) REFERENCES docs(id)`)
	require.NoError(t, err)
	_, err = pgvector.New[int64, string](ctx, db, schema)
	require.Error(t, err)
	_, err = db.ExecContext(ctx, `ALTER TABLE vectors_chunks DROP CONSTRAINT vectors_chunks_doc_key_fkey; ALTER TABLE vectors_chunks ADD FOREIGN KEY (doc_key) REFERENCES docs(id) ON DELETE CASCADE; INSERT INTO docs VALUES (1,'a',1)`)
	require.NoError(t, err)
	store, err := pgvector.New[int64, string](ctx, db, schema)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateBuilding))
	require.NoError(t, store.SaveVectors(ctx, "a", 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))
	_, err = db.ExecContext(ctx, `DELETE FROM docs WHERE id=1; INSERT INTO docs VALUES (1,'new',2)`)
	require.NoError(t, err)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM vectors_chunks`).Scan(&count))
	assert.Zero(t, count)
	hits, err := store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	assert.Empty(t, hits)
}
