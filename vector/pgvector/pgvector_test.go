package pgvector_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"math"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/sqlquery"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/pgvector"
)

type queryCounter struct{ count atomic.Int64 }

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.count.Add(1)
	return ctx
}

func (*queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func setup(t *testing.T) (*sql.DB, *pgvector.Store[int64, string]) {
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
	schema := pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "vectors"}
	require.NoError(t, pgvector.Install(t.Context(), db, schema))
	store, err := pgvector.New[int64, string](t.Context(), db, schema)
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

func TestEnsureGenerationRejectsUnknownState(t *testing.T) {
	_, store := setupActiveStore(t)
	ctx := t.Context()
	err := store.EnsureGeneration(ctx, "a", vector.Generation{Model: "a", Dimensions: 2}, vector.State("future"))
	require.EqualError(t, err, `invalid generation state "future"`)
	require.EqualError(t, store.EnsureGeneration(ctx, "c", vector.Generation{Dimensions: 2}, vector.StateRetired), `invalid generation state "retired"`)

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

func TestQueryGenerationWithPredicateFiltersBeforeLimit(t *testing.T) {
	db, store := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `ALTER TABLE docs ADD COLUMN project_id BIGINT`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO docs (id,body,revision,project_id) VALUES (1,'excluded',1,2),(2,'visible',7,1)`)
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
		require.FailNow(t, "query did not finish after releasing its table lock")
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

	_, err = store.PendingForGeneration(ctx, "a", nil, 10)
	require.ErrorIs(t, err, vector.ErrRetired)
	err = store.SaveVectors(ctx, "a", 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}})
	require.ErrorIs(t, err, vector.ErrRetired)
	require.ErrorIs(t, store.EnsureGeneration(ctx, "a", vector.Generation{Model: "a", Dimensions: 2}, vector.StateBuilding), vector.ErrRetired)
	require.ErrorIs(t, store.Activate(ctx, "a"), vector.ErrRetired)
}

func TestDirectVectorMethodsRejectInvalidVectors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		vector vector.Vector
		want   string
	}{
		{name: "zero norm", vector: vector.Vector{0, 0}, want: "zero norm"},
		{name: "NaN", vector: vector.Vector{1, float32(math.NaN())}, want: "non-finite"},
		{name: "infinity", vector: vector.Vector{1, float32(math.Inf(1))}, want: "non-finite"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store := setupActiveStore(t)
			ctx := t.Context()
			err := store.SaveVectors(ctx, "a", 2, 1, []vector.ChunkVector{{Vector: tc.vector}})
			require.ErrorContains(t, err, tc.want)
			_, err = store.QueryGeneration(ctx, "a", tc.vector, 10)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestActivateRequiresFullGenerationCoverage(t *testing.T) {
	_, store := setupActiveStore(t)
	ctx := t.Context()
	require.NoError(t, store.EnsureGeneration(ctx, "b", vector.Generation{Dimensions: 2}, vector.StateBuilding))
	require.NoError(t, store.SaveVectors(ctx, "b", 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))
	require.ErrorIs(t, store.Activate(ctx, "b"), vector.ErrUncovered)
	gens, err := store.LiveGenerations(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"b", "a"}, gens)

	require.NoError(t, store.SaveVectors(ctx, "b", 2, 1, []vector.ChunkVector{{Vector: vector.Vector{0, 1}}}))
	require.NoError(t, store.Activate(ctx, "b"))
	gens, err = store.LiveGenerations(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"b"}, gens)
}

func TestAbandonedGenerationStopsParticipatingInSearch(t *testing.T) {
	_, store := setupActiveStore(t)
	ctx := t.Context()
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = []float32{1, 0}
		}
		return vectors, nil
	}
	_, err := vector.Fill(ctx, store, "a", encode)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, "abandoned", vector.Generation{Dimensions: 2}, vector.StateBuilding))
	require.NoError(t, store.SetGenerationState(ctx, "abandoned", vector.StateRetired))
	hits, err := vector.Search(ctx, store, "cat", func(gen string) vector.EncodeFunc {
		if gen == "a" {
			return encode
		}
		return nil
	}, vector.SearchOptions{PerGeneration: 10})
	require.NoError(t, err)
	assert.Len(t, hits, 2)
	require.NoError(t, store.Reclaim(ctx, "abandoned"))
	require.ErrorIs(t, store.SetGenerationState(ctx, "abandoned", vector.StateBuilding), vector.ErrRetired)
	require.ErrorIs(t, store.SetGenerationState(ctx, "missing", vector.StateRetired), vector.ErrGenerationNotFound)
}

func TestSaveVectorsUsesConstantQueriesForChunks(t *testing.T) {
	db, _ := setupActiveStore(t)
	ctx := t.Context()
	var schemaName string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schemaName))
	config, err := pgx.ParseConfig(os.Getenv("KIT_POSTGRES_TEST_DSN"))
	require.NoError(t, err)
	config.RuntimeParams["search_path"] = schemaName
	counter := &queryCounter{}
	config.Tracer = counter
	traced := stdlib.OpenDB(*config)
	t.Cleanup(func() { assert.NoError(t, traced.Close()) })
	store, err := pgvector.New[int64, string](ctx, traced, pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "vectors"})
	require.NoError(t, err)
	one := []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}
	countBefore := counter.count.Load()
	require.NoError(t, store.SaveVectors(ctx, "a", 1, 1, one))
	oneCount := counter.count.Load() - countBefore
	many := []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}, {ChunkIndex: 1, Vector: vector.Vector{0, 1}}, {ChunkIndex: 2, Vector: vector.Vector{1, 1}}}
	countBefore = counter.count.Load()
	require.NoError(t, store.SaveVectors(ctx, "a", 1, 1, many))
	assert.Equal(t, oneCount, counter.count.Load()-countBefore)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM vectors_chunks_g1 WHERE doc_key=1`).Scan(&count))
	assert.Equal(t, 3, count)
}

func TestFillAndSearchDuringGenerationReplacement(t *testing.T) {
	_, store := setupActiveStore(t)
	ctx := t.Context()
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		vectors := make([][]float32, len(texts))
		for i, text := range texts {
			vectors[i] = []float32{1, 0}
			if text == "dog" {
				vectors[i] = []float32{0, 1}
			}
		}
		return vectors, nil
	}
	_, err := vector.Fill(ctx, store, "a", encode)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, "b", vector.Generation{Dimensions: 2}, vector.StateBuilding))
	_, err = vector.Fill(ctx, store, "b", encode, vector.WithFillDocumentLimit[int64](1))
	require.NoError(t, err)
	require.ErrorIs(t, store.Activate(ctx, "b"), vector.ErrUncovered)
	search := func() []vector.Hit[int64] {
		hits, err := vector.Search(ctx, store, "cat", func(string) vector.EncodeFunc { return encode }, vector.SearchOptions{PerGeneration: 10})
		require.NoError(t, err)
		return hits
	}
	hits := search()
	require.Len(t, hits, 2)
	assert.Equal(t, int64(1), hits[0].Doc)
	assert.Equal(t, int64(2), hits[1].Doc)
	_, err = vector.Fill(ctx, store, "b", encode)
	require.NoError(t, err)
	require.NoError(t, store.Activate(ctx, "b"))
	hits = search()
	require.Len(t, hits, 2)
	assert.Equal(t, int64(1), hits[0].Doc)
	assert.Equal(t, int64(2), hits[1].Doc)
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

func dmlOnly(t *testing.T, db *sql.DB) *sql.DB {
	t.Helper()
	ctx := t.Context()
	role := uniqueName(t, "vec_runtime")
	_, err := db.ExecContext(ctx, `CREATE ROLE `+role+` LOGIN PASSWORD 'test'`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.ExecContext(context.WithoutCancel(t.Context()), `DROP OWNED BY `+role+`; DROP ROLE `+role)
		assert.NoError(t, err)
	})
	var schemaName string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schemaName))
	_, err = db.ExecContext(ctx, `GRANT USAGE ON SCHEMA `+schemaName+` TO `+role+`; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA `+schemaName+` TO `+role+`; GRANT USAGE ON ALL SEQUENCES IN SCHEMA `+schemaName+` TO `+role)
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
	return runtime
}

func TestNewWithDMLOnlyRole(t *testing.T) {
	db, owner := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `INSERT INTO docs VALUES (1,'cat',1)`)
	require.NoError(t, err)
	require.NoError(t, owner.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateActive))
	store, err := pgvector.New[int64, string](ctx, dmlOnly(t, db), pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "vectors"})
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
	require.ErrorContains(t, pgvector.Install(t.Context(), db, schema), `prefix "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" is too long`)
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
	db, _ := setup(t)
	ctx := t.Context()
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

func TestSaveDocuments(t *testing.T) {
	db, store := setupActiveStore(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `UPDATE docs SET revision=2 WHERE id=2`)
	require.NoError(t, err)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	docs := []pgvector.Document[int64]{
		{Doc: 1, Revision: 1, Vectors: []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}},
		{Doc: 2, Revision: 1, Vectors: []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}},
		{Doc: 3, Revision: 1},
		{Doc: 2, Revision: 2, Vectors: []vector.ChunkVector{{Vector: vector.Vector{0, 1}}}},
		{Doc: 2, Revision: 1, Vectors: []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}},
	}
	stale, err := store.SaveDocuments(ctx, tx, "a", docs)
	require.NoError(t, err)
	require.Equal(t, []int64{2, 3, 2}, stale)
	require.NoError(t, tx.Commit())
	hits, err := store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	require.Equal(t, int64(1), hits[0].Doc)
	assert.Equal(t, int64(2), hits[1].Doc)
	assert.EqualValues(t, 2, hits[1].Revision)
	assert.InDelta(t, 0, hits[1].Score, 0.001)
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = store.SaveDocuments(ctx, tx, "a", []pgvector.Document[int64]{{Doc: 1, Revision: 1, Vectors: []vector.ChunkVector{{Vector: vector.Vector{1}}}}})
	require.ErrorContains(t, err, "save document 1")
	require.ErrorContains(t, err, "chunk 0")
	require.NoError(t, tx.Rollback())
}

func TestSaveDocumentsUUIDRevisions(t *testing.T) {
	db, _ := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `CREATE TABLE uuid_docs (id UUID PRIMARY KEY, body TEXT, revision TEXT); INSERT INTO uuid_docs VALUES ('abcdefab-cdef-abcd-efab-cdefabcdefab','cat',NULL),('12345678-1234-1234-1234-123456789abc','dog','new')`)
	require.NoError(t, err)
	schema := pgvector.Schema{DocsTable: "uuid_docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "uuid_vectors"}
	require.NoError(t, pgvector.Install(ctx, db, schema))
	store, err := pgvector.New[string, string](ctx, db, schema)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateActive))
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	stale, err := store.SaveDocuments(ctx, tx, "a", []pgvector.Document[string]{
		{Doc: "ABCDEFAB-CDEF-ABCD-EFAB-CDEFABCDEFAB", Revision: nil, Vectors: []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}},
		{Doc: "12345678-1234-1234-1234-123456789abc", Revision: "old", Vectors: []vector.ChunkVector{{Vector: vector.Vector{0, 1}}}},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"12345678-1234-1234-1234-123456789abc"}, stale)
	require.NoError(t, tx.Commit())
	hits, err := store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "abcdefab-cdef-abcd-efab-cdefabcdefab", hits[0].Doc)
}

func TestSaveDocumentsConcurrentSaves(t *testing.T) {
	for _, tc := range []struct {
		name   string
		older  bool
		single bool
	}{
		{name: "batch"},
		{name: "older revision", older: true},
		{name: "single save", single: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, store := setupActiveStore(t)
			ctx := t.Context()
			require.NoError(t, store.SaveVectors(ctx, "a", 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))
			first, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = first.Rollback() }()
			var pid int
			require.NoError(t, first.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
			revision := 1
			if tc.older {
				_, err = first.ExecContext(ctx, `UPDATE docs SET body='new', revision=2 WHERE id=1`)
				require.NoError(t, err)
				revision = 2
			}
			_, err = store.SaveDocuments(ctx, first, "a", []pgvector.Document[int64]{{Doc: 1, Revision: revision, Vectors: []vector.ChunkVector{{ChunkIndex: 1, Vector: vector.Vector{1, 0}}}}})
			require.NoError(t, err)
			type result struct {
				stale []int64
				err   error
			}
			done := make(chan result, 1)
			go func() {
				vectors := []vector.ChunkVector{{ChunkIndex: 2, Vector: vector.Vector{0, 1}}}
				if tc.single {
					done <- result{err: store.SaveVectors(ctx, "a", 1, 1, vectors)}
					return
				}
				second, err := db.BeginTx(ctx, nil)
				if err != nil {
					done <- result{err: err}
					return
				}
				defer func() { _ = second.Rollback() }()
				stale, err := store.SaveDocuments(ctx, second, "a", []pgvector.Document[int64]{{Doc: 1, Revision: 1, Vectors: vectors}})
				if err == nil {
					err = second.Commit()
				}
				done <- result{stale, err}
			}()
			require.Eventually(t, func() bool {
				var blocked bool
				err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked)
				return err == nil && blocked
			}, 5*time.Second, 10*time.Millisecond)
			require.NoError(t, first.Commit())
			r := <-done
			require.NoError(t, r.err)
			if tc.older {
				require.Equal(t, []int64{1}, r.stale)
			} else {
				require.Empty(t, r.stale)
			}
			hits, err := store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			if tc.older {
				require.Equal(t, 1, hits[0].ChunkIndex)
			} else {
				require.Equal(t, 2, hits[0].ChunkIndex)
			}
		})
	}
}

func TestSaveDocumentsWithRuntimeGrants(t *testing.T) {
	db, store := setupActiveStore(t)
	ctx := t.Context()
	role := uniqueName(t, "vec_runtime")
	_, err := db.ExecContext(ctx, `CREATE ROLE `+role)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.ExecContext(context.WithoutCancel(ctx), `DROP OWNED BY `+role+`; DROP ROLE `+role)
		assert.NoError(t, err)
	})
	var schema string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema))
	_, err = db.ExecContext(ctx, `GRANT USAGE ON SCHEMA `+schema+` TO `+role+`; GRANT SELECT, UPDATE ON docs TO `+role+`; GRANT SELECT ON vectors_generations TO `+role+`; GRANT SELECT, INSERT, UPDATE, DELETE ON vectors_stamps TO `+role+`; GRANT SELECT, INSERT, DELETE ON vectors_chunks_g1 TO `+role)
	require.NoError(t, err)
	var canUpdate bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT has_table_privilege($1,'vectors_generations','UPDATE')`, role).Scan(&canUpdate))
	require.False(t, canUpdate)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `SET LOCAL ROLE `+role)
	require.NoError(t, err)
	stale, err := store.SaveDocuments(ctx, tx, "a", []pgvector.Document[int64]{{Doc: 1, Revision: 1, Vectors: []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}}})
	require.NoError(t, err)
	require.Empty(t, stale)
	require.NoError(t, tx.Commit())
}

func TestSaveDocumentsTransactionWithEdit(t *testing.T) {
	for _, commit := range []bool{false, true} {
		name := "rollback"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			db, store := setupActiveStore(t)
			ctx := t.Context()
			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `UPDATE docs SET body='changed', revision=2 WHERE id=1`)
			require.NoError(t, err)
			stale, err := store.SaveDocuments(ctx, tx, "a", []pgvector.Document[int64]{{Doc: 1, Revision: 2, Vectors: []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}}})
			require.NoError(t, err)
			require.Empty(t, stale)
			if commit {
				require.NoError(t, tx.Commit())
			} else {
				require.NoError(t, tx.Rollback())
			}
			hits, err := store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
			require.NoError(t, err)
			pending, err := store.PendingForGeneration(ctx, "a", nil, 10)
			require.NoError(t, err)
			if commit {
				require.Len(t, hits, 1)
				require.Equal(t, int64(1), hits[0].Doc)
				require.Len(t, pending, 1)
			} else {
				require.Empty(t, hits)
				require.Len(t, pending, 2)
				require.Equal(t, "cat", pending[0].Content)
			}
		})
	}
}

func TestSaveDocumentsPreconditions(t *testing.T) {
	db, store := setupActiveStore(t)
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = store.SaveDocuments(ctx, tx, "missing", nil)
	require.ErrorIs(t, err, vector.ErrGenerationNotFound)
	require.NoError(t, tx.Rollback())
	require.NoError(t, store.SetGenerationState(ctx, "a", vector.StateRetired))
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = store.SaveDocuments(ctx, tx, "a", nil)
	require.ErrorIs(t, err, vector.ErrRetired)
	require.NoError(t, tx.Rollback())
	tx, err = db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	require.NoError(t, err)
	_, err = store.SaveDocuments(ctx, tx, "a", nil)
	require.ErrorContains(t, err, "requires READ COMMITTED")
	require.NoError(t, tx.Rollback())
}

func TestSaveDocumentsTwoGenerationsDuringActivation(t *testing.T) {
	db, store := setupActiveStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, store.EnsureGeneration(ctx, "z", vector.Generation{Model: "z", Dimensions: 2}, vector.StateBuilding))
	for _, id := range []int64{1, 2} {
		require.NoError(t, store.SaveVectors(ctx, "a", id, 1, nil))
	}
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var pid int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	_, err = tx.ExecContext(ctx, `UPDATE docs SET body='edited', revision=2 WHERE id=1`)
	require.NoError(t, err)
	stale, err := store.SaveDocuments(ctx, tx, "z", []pgvector.Document[int64]{{Doc: 1, Revision: 2}})
	require.NoError(t, err)
	require.Empty(t, stale)
	done := make(chan error, 1)
	go func() { done <- store.Activate(ctx, "a") }()
	var activationErr error
	finished := false
	require.Eventually(t, func() bool {
		select {
		case activationErr = <-done:
			finished = true
			return true
		default:
		}
		var blocked bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked)
		return err == nil && blocked
	}, 5*time.Second, 10*time.Millisecond)
	stale, err = store.SaveDocuments(ctx, tx, "a", []pgvector.Document[int64]{{Doc: 1, Revision: 2}})
	var busy *pgvector.GenerationBusyError
	require.ErrorAs(t, err, &busy)
	require.NoError(t, tx.Rollback())
	if !finished {
		activationErr = <-done
	}
	require.NoError(t, activationErr)
	retry, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = retry.Rollback() }()
	_, err = retry.ExecContext(ctx, `UPDATE docs SET body='edited', revision=2 WHERE id=1`)
	require.NoError(t, err)
	stale, err = store.SaveDocuments(ctx, retry, "a", []pgvector.Document[int64]{{Doc: 1, Revision: 2, Vectors: []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}}})
	require.NoError(t, err)
	require.Empty(t, stale)
	require.NoError(t, retry.Commit())
	hits, err := store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(1), hits[0].Doc)
	assert.EqualValues(t, 2, hits[0].Revision)
}

func TestSaveDocumentsRetiredWhileWaitingForDocument(t *testing.T) {
	db, store := setupActiveStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, store.EnsureGeneration(ctx, "b", vector.Generation{Model: "b", Dimensions: 2}, vector.StateActive))
	blocker, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.ExecContext(ctx, `UPDATE docs SET body='changed' WHERE id=1`)
	require.NoError(t, err)
	var pid int
	require.NoError(t, blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	done := make(chan error, 1)
	go func() {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = tx.Rollback() }()
		_, err = store.SaveDocuments(ctx, tx, "a", []pgvector.Document[int64]{{Doc: 1, Revision: 1}})
		done <- err
	}()
	require.Eventually(t, func() bool {
		var blocked bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked)
		return err == nil && blocked
	}, 5*time.Second, 10*time.Millisecond)
	retired := make(chan error, 1)
	go func() { retired <- store.SetGenerationState(ctx, "a", vector.StateRetired) }()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT FROM pg_stat_activity WHERE wait_event='advisory' AND query LIKE '%pg_advisory_xact_lock%')`).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond)
	otherCtx, otherCancel := context.WithTimeout(ctx, 2*time.Second)
	defer otherCancel()
	second, err := db.BeginTx(otherCtx, nil)
	require.NoError(t, err)
	defer func() { _ = second.Rollback() }()
	_, err = store.SaveDocuments(otherCtx, second, "b", []pgvector.Document[int64]{{Doc: 2, Revision: 1, Vectors: []vector.ChunkVector{{Vector: vector.Vector{0, 1}}}}})
	require.NoError(t, err)
	require.NoError(t, second.Commit())
	require.NoError(t, blocker.Commit())
	require.NoError(t, <-done)
	require.NoError(t, <-retired)
}

func TestSaveDocumentsReclaimWithCallerDocumentLock(t *testing.T) {
	db, store := setupActiveStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `UPDATE docs SET body='changed' WHERE id=1`)
	require.NoError(t, err)
	var pid int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	require.NoError(t, store.SetGenerationState(ctx, "a", vector.StateRetired))
	done := make(chan error, 1)
	go func() { done <- store.Reclaim(ctx, "a") }()
	require.Eventually(t, func() bool {
		var blocked bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked)
		return err == nil && blocked
	}, 5*time.Second, 10*time.Millisecond)
	_, err = store.SaveDocuments(ctx, tx, "a", []pgvector.Document[int64]{{Doc: 1, Revision: 1}})
	rollbackErr := tx.Rollback()
	reclaimErr := <-done
	var busy *pgvector.GenerationBusyError
	assert.ErrorAs(t, err, &busy)
	assert.NoError(t, rollbackErr)
	assert.NoError(t, reclaimErr)
}
