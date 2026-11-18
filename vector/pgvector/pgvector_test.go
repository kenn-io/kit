package pgvector_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/pgvector"
)

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
	name := fmt.Sprintf("vec_test_%d", os.Getpid())
	_, err = admin.ExecContext(t.Context(), `CREATE SCHEMA `+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.Background(), `DROP SCHEMA `+name+` CASCADE`)
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
func encoder(dim int, calls *int) vector.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		*calls += len(texts)
		out := make([][]float32, len(texts))
		for i, text := range texts {
			out[i] = make([]float32, dim)
			if strings.Contains(text, "cat") {
				out[i][0] = 1
			} else {
				out[i][1] = 1
			}
		}
		return out, nil
	}
}

func TestFillSearchAndGenerationLifecycle(t *testing.T) {
	db, s := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `INSERT INTO docs VALUES (1,'cat',1),(2,'dog',1),(3,' ',1)`)
	require.NoError(t, err)
	require.NoError(t, s.EnsureGeneration(ctx, "a", vector.Generation{Model: "a", Dimensions: 2}, vector.StateActive))
	calls := 0
	stats, err := vector.Fill(ctx, s, "a", encoder(2, &calls))
	require.NoError(t, err)
	assert.Equal(t, 3, stats.Documents)
	assert.Equal(t, 2, calls)
	_, err = vector.Fill(ctx, s, "a", encoder(2, &calls))
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	hits, err := vector.Search(ctx, s, "cat", func(string) vector.EncodeFunc { return encoder(2, &calls) }, vector.SearchOptions{})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, int64(1), hits[0].Doc)
	require.NoError(t, s.EnsureGeneration(ctx, "b", vector.Generation{Model: "b", Dimensions: 3}, vector.StateBuilding))
	require.NoError(t, s.SaveVectors(ctx, "b", 1, 1, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))
	hits, err = vector.Search(ctx, s, "cat", func(gen string) vector.EncodeFunc {
		if gen == "b" {
			return encoder(3, &calls)
		}
		return encoder(2, &calls)
	}, vector.SearchOptions{})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, int64(1), hits[0].Doc)
	require.ErrorIs(t, s.Activate(ctx, "b"), vector.ErrUncovered)
	_, err = vector.Fill(ctx, s, "b", encoder(3, &calls))
	require.NoError(t, err)
	require.NoError(t, s.Activate(ctx, "b"))
	require.NoError(t, s.Reclaim(ctx, "a"))
	var exists bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass('vectors_chunks_g1') IS NOT NULL`).Scan(&exists))
	assert.False(t, exists)
}

func TestStaleEditedDeleted(t *testing.T) {
	db, s := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `INSERT INTO docs VALUES (1,'cat',1),(2,'dog',1)`)
	require.NoError(t, err)
	require.NoError(t, s.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateActive))
	pending, err := s.PendingForGeneration(ctx, "a", nil, 10)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	_, err = db.ExecContext(ctx, `UPDATE docs SET revision=2 WHERE id=1`)
	require.NoError(t, err)
	require.ErrorIs(t, s.SaveVectors(ctx, "a", 1, pending[0].Revision, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}), vector.ErrStale)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM vectors_chunks_g1`).Scan(&count))
	assert.Zero(t, count)
	calls := 0
	_, err = vector.Fill(ctx, s, "a", encoder(2, &calls))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE docs SET revision=3 WHERE id=1; DELETE FROM docs WHERE id=2`)
	require.NoError(t, err)
	hits, err := s.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	assert.Empty(t, hits)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM vectors_chunks_g1 WHERE doc_key=2`).Scan(&count))
	assert.Zero(t, count)
	_, err = vector.Fill(ctx, s, "a", encoder(2, &calls))
	require.NoError(t, err)
	hits, err = s.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(1), hits[0].Doc)
}

func TestSearchSkipsStaleNearestNeighbors(t *testing.T) {
	db, s := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `INSERT INTO docs SELECT i, 'doc', 1 FROM generate_series(1,160) i`)
	require.NoError(t, err)
	require.NoError(t, s.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateActive))
	for i := int64(1); i <= 160; i++ {
		v := vector.Vector{0.5, 0.5}
		if i <= 80 {
			v = vector.Vector{1, 0}
		}
		require.NoError(t, s.SaveVectors(ctx, "a", i, 1, []vector.ChunkVector{{Vector: v}}))
	}
	_, err = db.ExecContext(ctx, `UPDATE docs SET revision=2 WHERE id<=80; ANALYZE vectors_chunks_g1`)
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
	_, err = db.ExecContext(ctx, `CREATE ROLE vec_runtime LOGIN PASSWORD 'test'`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.ExecContext(context.Background(), `DROP OWNED BY vec_runtime; DROP ROLE vec_runtime`)
		assert.NoError(t, err)
	})
	var schemaName string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schemaName))
	_, err = db.ExecContext(ctx, `GRANT USAGE ON SCHEMA `+schemaName+` TO vec_runtime; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA `+schemaName+` TO vec_runtime`)
	require.NoError(t, err)
	u, err := url.Parse(os.Getenv("KIT_POSTGRES_TEST_DSN"))
	require.NoError(t, err)
	u.User = url.UserPassword("vec_runtime", "test")
	q := u.Query()
	q.Set("search_path", schemaName)
	u.RawQuery = q.Encode()
	runtime, err := sql.Open("pgx", u.String())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })
	store, err := pgvector.New[int64, string](ctx, runtime, pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "vectors"})
	require.NoError(t, err)
	require.NoError(t, store.SaveVectors(ctx, "a", 1, 1, []vector.ChunkVector{{Vector: vector.Vector{1, 0}}}))
	hits, err := store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
}

func TestInvalidStatesAndActiveGeneration(t *testing.T) {
	_, s := setup(t)
	ctx := t.Context()
	_, ok, err := s.ActiveGeneration(ctx)
	require.NoError(t, err)
	assert.False(t, ok)
	model := vector.Generation{Model: "model", Dimensions: 2}
	require.Error(t, s.EnsureGeneration(ctx, "bad", model, vector.State("buildingg")))
	require.NoError(t, s.EnsureGeneration(ctx, "a", model, vector.StateActive))
	require.Error(t, s.SetGenerationState(ctx, "a", vector.State("buildingg")))
	info, ok, err := s.ActiveGeneration(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "a", info.Key)
	assert.Equal(t, model.Fingerprint(), info.Fingerprint)
	assert.Equal(t, 2, info.Dimension)
	assert.Equal(t, vector.StateActive, info.State)
}

func TestSchemaRejectsTruncatedNames(t *testing.T) {
	db, _ := setup(t)
	schema := pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: strings.Repeat("x", 31)}
	require.Error(t, pgvector.Install(t.Context(), db, schema))
	schema.Prefix = strings.Repeat("x", 30)
	require.NoError(t, pgvector.Install(t.Context(), db, schema))
	schema.IDColumn = strings.Repeat("x", 64)
	require.Error(t, pgvector.Install(t.Context(), db, schema))
}

func TestGenerationKeyTypeAliases(t *testing.T) {
	db, _ := setup(t)
	ctx := t.Context()
	for _, typ := range []string{"bigint", "integer"} {
		schema := pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "v_" + typ, GenerationKeyType: typ}
		require.NoError(t, pgvector.Install(ctx, db, schema))
		store, err := pgvector.New[int64, int64](ctx, db, schema)
		require.NoError(t, err)
		require.NoError(t, store.EnsureGeneration(ctx, 42, vector.Generation{Dimensions: 2}, vector.StateActive))
		info, ok, err := store.ActiveGeneration(ctx)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, int64(42), info.Key)
	}
}

func TestFillContinuesAfterDocumentDeleted(t *testing.T) {
	db, s := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `INSERT INTO docs VALUES (1,'cat',1),(2,'dog',1)`)
	require.NoError(t, err)
	require.NoError(t, s.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateActive))
	calls := 0
	enc := encoder(2, &calls)
	_, err = vector.Fill(ctx, s, "a", func(ctx context.Context, texts []string) ([][]float32, error) {
		_, err := db.ExecContext(ctx, `DELETE FROM docs WHERE id=1`)
		if err != nil {
			return nil, err
		}
		return enc(ctx, texts)
	})
	require.NoError(t, err)
	hits, err := s.QueryGeneration(ctx, "a", vector.Vector{0, 1}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(2), hits[0].Doc)
}

func TestQuotedExtensionSchema(t *testing.T) {
	db, _ := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `CREATE SCHEMA "vector-""ext"; ALTER EXTENSION vector SET SCHEMA "vector-""ext"`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.ExecContext(context.Background(), `ALTER EXTENSION vector SET SCHEMA public; DROP SCHEMA "vector-""ext"`)
		assert.NoError(t, err)
	})
	schema := pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "vectors"}
	store, err := pgvector.New[int64, string](ctx, db, schema)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO docs VALUES (1,'cat',1)`)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateActive))
	calls := 0
	_, err = vector.Fill(ctx, store, "a", encoder(2, &calls))
	require.NoError(t, err)
	hits, err := store.QueryGeneration(ctx, "a", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(1), hits[0].Doc)
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

func TestSearchRejectsUnknownGenerationState(t *testing.T) {
	db, store := setup(t)
	ctx := t.Context()
	require.NoError(t, store.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateActive))
	_, err := db.ExecContext(ctx, `ALTER TABLE vectors_generations DROP CONSTRAINT vectors_state_check; UPDATE vectors_generations SET state='future' WHERE gen_key='a'`)
	require.NoError(t, err)
	calls := 0
	_, err = vector.Search(ctx, store, "cat", func(string) vector.EncodeFunc { return encoder(2, &calls) }, vector.SearchOptions{})
	require.ErrorContains(t, err, "future")
	assert.Zero(t, calls)
	_, _, err = store.ActiveGeneration(ctx)
	require.ErrorContains(t, err, "future")
	require.ErrorContains(t, store.EnsureGeneration(ctx, "a", vector.Generation{Dimensions: 2}, vector.StateActive), "future")
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
				_, err := db.ExecContext(context.Background(), `DROP TABLE broken_generations, broken_stamps`)
				assert.NoError(t, err)
			})
			schema := pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "broken"}
			_, err = pgvector.New[int64, string](ctx, db, schema)
			require.ErrorContains(t, err, col)
			require.ErrorContains(t, pgvector.Install(ctx, db, schema), col)
		})
	}
}

func TestStateConstraint(t *testing.T) {
	db, _ := setup(t)
	_, err := db.ExecContext(t.Context(), `INSERT INTO vectors_generations (gen_key,fingerprint,dimension,state) VALUES ('x','x',2,'future')`)
	require.Error(t, err)
}

func TestStampDeleteUsesDocumentIndex(t *testing.T) {
	db, _ := setup(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `INSERT INTO docs SELECT i, 'doc', 1 FROM generate_series(1,10000) i; INSERT INTO vectors_stamps SELECT 1, i, 1 FROM generate_series(1,10000) i; ANALYZE vectors_stamps`)
	require.NoError(t, err)
	rows, err := db.QueryContext(ctx, `EXPLAIN DELETE FROM vectors_stamps WHERE doc_key=5000`)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	var plan string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		plan += line
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, plan, "vectors_stamps_by_doc")
}

func TestOldExtensionVersionRejected(t *testing.T) {
	db, _ := setup(t)
	ctx := t.Context()
	var version string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT extversion FROM pg_extension WHERE extname='vector'`).Scan(&version))
	_, err := db.ExecContext(ctx, `UPDATE pg_extension SET extversion='0.7.4' WHERE extname='vector'`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.ExecContext(context.Background(), `UPDATE pg_extension SET extversion=$1 WHERE extname='vector'`, version)
		assert.NoError(t, err)
	})
	schema := pgvector.Schema{DocsTable: "docs", IDColumn: "id", ContentColumn: "body", RevisionColumn: "revision", Prefix: "vectors"}
	_, err = pgvector.New[int64, string](ctx, db, schema)
	require.ErrorContains(t, err, "0.8.0")
	require.ErrorContains(t, pgvector.Install(ctx, db, schema), "0.8.0")
}
