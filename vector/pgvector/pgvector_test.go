package pgvector_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/sqlquery"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/pgvector"
)

func setup(t *testing.T) (*sql.DB, *pgvector.Store[int64, string]) {
	t.Helper()
	return setupWithGenerationKeyType[string](t, "")
}

func setupWithGenerationKeyType[G comparable](t *testing.T, generationKeyType string) (*sql.DB, *pgvector.Store[int64, G]) {
	t.Helper()
	dsn := os.Getenv("KIT_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("KIT_POSTGRES_TEST_DSN is not set")
	}
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	_, err = admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector`)
	require.NoError(t, err)
	name := uniqueName(t, "vec_test")
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
	_, err = db.ExecContext(t.Context(), `CREATE TABLE docs (id BIGINT PRIMARY KEY, body TEXT, revision INTEGER)`)
	require.NoError(t, err)
	schema := pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "vectors", GenerationKeyType: generationKeyType}
	require.NoError(t, pgvector.Install(t.Context(), db, schema))
	store, err := pgvector.New[int64, G](t.Context(), db, schema)
	require.NoError(t, err)
	return db, store
}

func uniqueName(t *testing.T, prefix string) string {
	t.Helper()
	var suffix [16]byte
	_, err := rand.Read(suffix[:])
	require.NoError(t, err)
	return prefix + "_" + hex.EncodeToString(suffix[:])
}

func explain(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "EXPLAIN "+query)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		plan.WriteString(line)
	}
	require.NoError(t, rows.Err())
	return plan.String()
}

func setupActiveStore(t *testing.T) (*sql.DB, *pgvector.Store[int64, string]) {
	t.Helper()
	db, store := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `INSERT INTO docs VALUES (1,'cat',1),(2,'dog',1)`)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, "a", vector.Generation{Model: "a", Dimensions: 2}, vector.StateActive))
	return db, store
}

func TestFillSearchAndGenerationLifecycle(t *testing.T) {
	db, s := setupActiveStore(t)
	ctx := t.Context()
	pending, err := s.PendingForGeneration(ctx, "a", nil, 10)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	for _, doc := range pending {
		embedding := vector.Vector{0, 1}
		if doc.Doc == 1 {
			embedding = vector.Vector{1, 0}
		}
		require.NoError(t, s.SaveVectors(ctx, "a", doc.Doc, doc.Revision, []vector.ChunkVector{{Vector: embedding}}))
	}
	hits, err := s.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, int64(1), hits[0].Doc)
	model := vector.Generation{Model: "b", Dimensions: 3}
	require.NoError(t, s.EnsureGeneration(ctx, "b", model, vector.StateBuilding))
	require.NoError(t, s.SaveVectors(ctx, "b", 1, 1, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))
	require.ErrorIs(t, s.Activate(ctx, "b"), vector.ErrUncovered)
	pending, err = s.PendingForGeneration(ctx, "b", nil, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, int64(2), pending[0].Doc)
	require.NoError(t, s.SaveVectors(ctx, "b", pending[0].Doc, pending[0].Revision, []vector.ChunkVector{{Vector: vector.Vector{0, 1, 0}}}))
	gens, err := s.LiveGenerations(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"b", "a"}, gens)
	// Both generations remain queryable with their own dimensions until activation.
	oldHits, err := s.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, oldHits, 2)
	assert.Equal(t, int64(1), oldHits[0].Doc)
	newHits, err := s.QueryGeneration(ctx, "b", vector.Vector{1, 0, 0}, 10)
	require.NoError(t, err)
	require.Len(t, newHits, 2)
	assert.Equal(t, int64(1), newHits[0].Doc)
	require.NoError(t, s.Activate(ctx, "b"))
	gens, err = s.LiveGenerations(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"b"}, gens)
	var state string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT state FROM vectors_generations WHERE gen_key='a'`).Scan(&state))
	assert.Equal(t, "retired", state)
	// The retired generation keeps its storage and stays queryable for in-flight searches.
	oldHits, err = s.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, oldHits, 2)
	require.EqualError(t, s.EnsureGeneration(ctx, "c", vector.Generation{Dimensions: 2}, vector.StateRetired), `invalid generation state "retired"`)
	require.ErrorIs(t, s.EnsureGeneration(ctx, "a", vector.Generation{Model: "a", Dimensions: 2}, vector.StateBuilding), vector.ErrRetired)
	require.ErrorIs(t, s.Activate(ctx, "a"), vector.ErrRetired)
}

func TestEnsureGenerationRejectsUnknownState(t *testing.T) {
	_, store := setupActiveStore(t)
	ctx := t.Context()
	err := store.EnsureGeneration(ctx, "a", vector.Generation{Model: "a", Dimensions: 2}, vector.State("future"))
	require.EqualError(t, err, `invalid generation state "future"`)

	gens, err := store.LiveGenerations(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, gens)
}

func TestUnknownPersistedGenerationStateIsRejected(t *testing.T) {
	db, store := setupActiveStore(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `UPDATE vectors_generations SET state='future' WHERE gen_key='a'`)
	require.NoError(t, err)

	_, err = store.LiveGenerations(ctx)
	require.ErrorContains(t, err, `unknown generation state "future"`)
	_, err = store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.ErrorContains(t, err, `unknown generation state "future"`)
}

func TestNumericGenerationKey(t *testing.T) {
	db, store := setupWithGenerationKeyType[int64](t, "bigint")
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `INSERT INTO docs VALUES (1,'cat',1)`)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, 42, vector.Generation{Model: "numeric", Dimensions: 2}, vector.StateActive))
	require.NoError(t, store.SaveVectors(ctx, 42, 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))

	gens, err := store.LiveGenerations(ctx)
	require.NoError(t, err)
	assert.Equal(t, []int64{42}, gens)
	hits, err := store.QueryGeneration(ctx, 42, vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(1), hits[0].Doc)
}

func TestQueryGenerationWithPredicateFiltersBeforeLimit(t *testing.T) {
	db, store := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `ALTER TABLE docs ADD COLUMN project_id BIGINT`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO docs (id,body,revision,project_id) VALUES (1,'private',1,2),(2,'visible',7,1)`)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateActive))
	require.NoError(t, store.SaveVectors(ctx, "a", 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))
	require.NoError(t, store.SaveVectors(ctx, "a", 2, 7, []vector.ChunkVector{{Vector: vector.Vector{0, 1}}}))

	hits, err := store.QueryGenerationWithPredicate(ctx, "a", vector.Vector{1, 0}, 1, sqlquery.Predicate{
		SQL:  "d.project_id = ?",
		Args: []any{int64(1)},
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(2), hits[0].Doc)
	assert.EqualValues(t, 7, hits[0].Revision)
}

func TestReclaimWaitsForQueryAndHandlesStaleGenerationList(t *testing.T) {
	db, store := setupActiveStore(t)
	db.SetMaxOpenConns(8)
	ctx := t.Context()

	pending, err := store.PendingForGeneration(ctx, "a", nil, 10)
	require.NoError(t, err)
	for _, doc := range pending {
		require.NoError(t, store.SaveVectors(ctx, "a", doc.Doc, doc.Revision, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))
	}
	require.NoError(t, store.EnsureGeneration(ctx, "b", vector.Generation{Dimensions: 2}, vector.StateBuilding))
	pending, err = store.PendingForGeneration(ctx, "b", nil, 10)
	require.NoError(t, err)
	for _, doc := range pending {
		require.NoError(t, store.SaveVectors(ctx, "b", doc.Doc, doc.Revision, []vector.ChunkVector{{Vector: vector.Vector{0, 1}}}))
	}
	oldList, err := store.LiveGenerations(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"b", "a"}, oldList)
	oldGen := oldList[1]
	require.NoError(t, store.Activate(ctx, "b"))

	blocker, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	_, err = blocker.ExecContext(ctx, `LOCK TABLE vectors_chunks_g1 IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)

	type queryResult struct {
		hits []vector.Hit[int64]
		err  error
	}
	queryDone := make(chan queryResult, 1)
	go func() {
		hits, err := store.QueryGeneration(ctx, oldGen, vector.Vector{1, 0}, 10)
		queryDone <- queryResult{hits: hits, err: err}
	}()
	waitForActivity := func(event, query string) {
		t.Helper()
		require.Eventually(t, func() bool {
			var waiting int
			err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND wait_event=$1 AND strpos(query,$2)>0`, event, query).Scan(&waiting)
			return err == nil && waiting > 0
		}, 5*time.Second, 10*time.Millisecond)
	}
	waitForActivity("relation", `LOCK TABLE "vectors_chunks_g1" IN ACCESS SHARE MODE`)

	reclaimDone := make(chan error, 1)
	go func() { reclaimDone <- store.Reclaim(ctx, oldGen) }()
	waitForActivity("relation", `DROP TABLE IF EXISTS "vectors_chunks_g1"`)
	require.NoError(t, blocker.Rollback())

	select {
	case result := <-queryDone:
		require.NoError(t, result.err)
		require.Len(t, result.hits, 2)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "query did not finish after releasing its advisory lock")
	}
	select {
	case err := <-reclaimDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "reclaim did not finish after the query released its table lock")
	}

	hits, err := store.QueryGeneration(ctx, oldGen, vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	assert.Empty(t, hits)
	var chunksExist bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass('vectors_chunks_g1') IS NOT NULL`).Scan(&chunksExist))
	assert.False(t, chunksExist)
	var stampCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM vectors_stamps WHERE generation_id=(SELECT id FROM vectors_generations WHERE gen_key=$1)`, oldGen).Scan(&stampCount))
	assert.Zero(t, stampCount)
}

func TestStaleEdited(t *testing.T) {
	db, s := setupActiveStore(t)
	ctx := t.Context()
	pending, err := s.PendingForGeneration(ctx, "a", nil, 10)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	assert.Equal(t, int64(1), pending[0].Doc)
	_, err = db.ExecContext(ctx, `UPDATE docs SET body='cat updated', revision=2 WHERE id=1`)
	require.NoError(t, err)
	require.ErrorIs(t, s.SaveVectors(ctx, "a", 1, pending[0].Revision, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}), vector.ErrStale)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM vectors_chunks_g1`).Scan(&count))
	assert.Zero(t, count)
	pending, err = s.PendingForGeneration(ctx, "a", nil, 10)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	assert.Equal(t, int64(1), pending[0].Doc)
	assert.EqualValues(t, 2, pending[0].Revision)
	assert.Equal(t, "cat updated", pending[0].Content)
	for _, doc := range pending {
		embedding := vector.Vector{0, 1}
		if doc.Doc == 1 {
			embedding = vector.Vector{1, 0}
		}
		require.NoError(t, s.SaveVectors(ctx, "a", doc.Doc, doc.Revision, []vector.ChunkVector{{Vector: embedding}}))
	}
	hits, err := s.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, int64(1), hits[0].Doc)
	assert.EqualValues(t, 2, hits[0].Revision)
}

func TestSearchSkipsStaleNearestNeighbors(t *testing.T) {
	db, s := setup(t)
	db.SetMaxOpenConns(1)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `SET enable_seqscan = off; SET enable_sort = off; SET enable_mergejoin = off; SET enable_hashjoin = off`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO docs SELECT i, 'doc', 1 FROM generate_series(1,160) i`)
	require.NoError(t, err)
	require.NoError(t, s.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateActive))
	for i := int64(1); i <= 160; i++ {
		v := vector.Vector{0.5, 0.5}
		if i <= 80 {
			v = vector.Vector{1, 0}
		}
		require.NoError(t, s.SaveVectors(ctx, "a", i, 1, []vector.ChunkVector{{Vector: v}}))
	}
	_, err = db.ExecContext(ctx, `UPDATE docs SET revision=2 WHERE id<=80; ANALYZE docs; ANALYZE vectors_stamps; ANALYZE vectors_chunks_g1`)
	require.NoError(t, err)
	hits, err := s.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 60)
	require.NoError(t, err)
	require.Len(t, hits, 60)
	for _, hit := range hits {
		assert.Greater(t, hit.Doc, int64(80))
	}
}

func TestNewWithDMLOnlyRole(t *testing.T) {
	db, owner := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `INSERT INTO docs VALUES (1,'cat',1)`)
	require.NoError(t, err)
	require.NoError(t, owner.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateActive))
	role := uniqueName(t, "vec_runtime")
	_, err = db.ExecContext(ctx, `CREATE ROLE `+role+` LOGIN PASSWORD 'test'`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.ExecContext(context.WithoutCancel(t.Context()), `DROP OWNED BY `+role+`; DROP ROLE `+role)
		assert.NoError(t, err)
	})
	var schemaName string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schemaName))
	_, err = db.ExecContext(ctx, `GRANT USAGE ON SCHEMA `+schemaName+` TO `+role+`; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA `+schemaName+` TO `+role)
	require.NoError(t, err)
	u, err := url.Parse(os.Getenv("KIT_POSTGRES_TEST_DSN"))
	require.NoError(t, err)
	u.User = url.UserPassword(role, "test")
	q := u.Query()
	q.Set("search_path", schemaName)
	u.RawQuery = q.Encode()
	runtime, err := sql.Open("pgx", u.String())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })
	store, err := pgvector.New[int64, string](ctx, runtime, pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "vectors"})
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateActive))
	require.NoError(t, store.SaveVectors(ctx, "a", 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))
	hits, err := store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
}

func TestSchemaRejectsLongPrefix(t *testing.T) {
	db, _ := setup(t)
	schema := pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: strings.Repeat("x", 31)}
	require.Error(t, pgvector.Install(t.Context(), db, schema))
	schema.Prefix = strings.Repeat("x", 30)
	require.NoError(t, pgvector.Install(t.Context(), db, schema))
	store, err := pgvector.New[int64, string](t.Context(), db, schema)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(t.Context(), "a", vector.Generation{Dimensions: 2}, vector.StateBuilding))
}

func TestHalfvecPreservesDirectionAcrossFloat32Scales(t *testing.T) {
	db, store := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `INSERT INTO docs VALUES (1,'one',1),(2,'two',1),(3,'three',1)`)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateActive))
	for _, tc := range []struct {
		doc       int64
		embedding vector.Vector
	}{
		{1, vector.Vector{1e-8, 0}}, {2, vector.Vector{0, 1e30}}, {3, vector.Vector{-1e-40, 0}},
	} {
		require.NoError(t, store.SaveVectors(ctx, "a", tc.doc, 1, []vector.ChunkVector{{Vector: tc.embedding}}))
	}
	for _, query := range []vector.Vector{{1e-8, 0}, {1e30, 0}, {1e-40, 0}} {
		hits, err := store.QueryGeneration(ctx, "a", query, 3)
		require.NoError(t, err)
		require.Len(t, hits, 3)
		assert.Equal(t, []int64{1, 2, 3}, []int64{hits[0].Doc, hits[1].Doc, hits[2].Doc})
		assert.InDelta(t, 1, hits[0].Score, 0.001)
		assert.InDelta(t, 0, hits[1].Score, 0.001)
		assert.InDelta(t, -1, hits[2].Score, 0.001)
	}
}

func TestSourceDeletionCascades(t *testing.T) {
	db, store := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `INSERT INTO docs VALUES (1,'cat',1)`)
	require.NoError(t, err)
	for _, gen := range []string{"a", "b"} {
		require.NoError(t, store.EnsureGeneration(ctx, gen, vector.Generation{Model: gen, Dimensions: 2}, vector.StateBuilding))
		require.NoError(t, store.SaveVectors(ctx, gen, 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))
	}
	_, err = db.ExecContext(ctx, `DELETE FROM docs WHERE id=1`)
	require.NoError(t, err)
	for _, table := range []string{"vectors_stamps", "vectors_chunks_g1", "vectors_chunks_g2"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&count))
		assert.Zero(t, count, table)
	}
	require.ErrorIs(t, store.SaveVectors(ctx, "a", 1, 1, nil), vector.ErrStale)
	_, err = db.ExecContext(ctx, `INSERT INTO docs VALUES (1,'new',1)`)
	require.NoError(t, err)
	pending, err := store.PendingForGeneration(ctx, "a", nil, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	hits, err := store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	assert.Empty(t, hits)
}

func TestIncompatibleCatalog(t *testing.T) {
	db, _ := setup(t)
	ctx := t.Context()
	for _, col := range []string{"gen_key", "state"} {
		t.Run(col, func(t *testing.T) {
			_, err := db.ExecContext(ctx, `CREATE TABLE broken_generations (id bigint, fingerprint text, dimension integer, gen_key text, state text); CREATE TABLE broken_stamps (generation_id bigint, doc_key bigint, revision integer); ALTER TABLE broken_generations DROP COLUMN `+col)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := db.ExecContext(context.WithoutCancel(t.Context()), `DROP TABLE broken_generations, broken_stamps`)
				assert.NoError(t, err)
			})
			schema := pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "broken"}
			_, err = pgvector.New[int64, string](ctx, db, schema)
			require.ErrorContains(t, err, col)
			require.ErrorContains(t, pgvector.Install(ctx, db, schema), col)
		})
	}
}

func TestStampDeleteUsesDocumentIndex(t *testing.T) {
	db, store := setup(t)
	ctx := t.Context()
	require.NoError(t, store.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateBuilding))
	_, err := db.ExecContext(ctx, `INSERT INTO docs SELECT i, 'doc', 1 FROM generate_series(1,10000) i; INSERT INTO vectors_stamps SELECT 1, i, 1 FROM generate_series(1,10000) i; ANALYZE vectors_stamps`)
	require.NoError(t, err)
	plan := explain(t, db, `DELETE FROM vectors_stamps WHERE doc_key=5000`)
	assert.Contains(t, plan, "vectors_stamps_by_doc")
}

// TestQuotedExtensionSchema runs in its own database so moving the extension never touches the shared test database.
func TestQuotedExtensionSchema(t *testing.T) {
	dsn := os.Getenv("KIT_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("KIT_POSTGRES_TEST_DSN is not set")
	}
	ctx := t.Context()
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, admin.Close()) })
	name := uniqueName(t, "vec_ext")
	_, err = admin.ExecContext(ctx, `CREATE DATABASE `+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.WithoutCancel(ctx), `DROP DATABASE `+name+` WITH (FORCE)`)
		assert.NoError(t, err)
	})
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.Path = "/" + name
	db, err := sql.Open("pgx", u.String())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	_, err = db.ExecContext(ctx, `CREATE SCHEMA "vector-""ext"; CREATE EXTENSION vector SCHEMA "vector-""ext"; CREATE TABLE docs (id BIGINT PRIMARY KEY, body TEXT, revision INTEGER); INSERT INTO docs VALUES (1,'cat',1)`)
	require.NoError(t, err)
	schema := pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "vectors"}
	require.NoError(t, pgvector.Install(ctx, db, schema))
	store, err := pgvector.New[int64, string](ctx, db, schema)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateActive))
	require.NoError(t, store.SaveVectors(ctx, "a", 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))
	hits, err := store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(1), hits[0].Doc)
}
