package sqlitevec_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// topicEncoder maps text to a one-hot 3-D vector by keyword, so queries
// match documents deterministically.
func topicEncoder() vector.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			switch {
			case strings.Contains(text, "cat"):
				out[i] = []float32{1, 0, 0}
			case strings.Contains(text, "dog"):
				out[i] = []float32{0, 1, 0}
			default:
				out[i] = []float32{0, 0, 1}
			}
		}
		return out, nil
	}
}

func setup(t *testing.T) (*sql.DB, *sqlitevec.Store[int64, int64]) {
	t.Helper()
	db, err := openSQLiteTestDB(t, filepath.Join(t.TempDir(), "vec.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	_, err = db.Exec(`CREATE TABLE messages (id INTEGER PRIMARY KEY, body TEXT, embed_gen INTEGER)`)
	require.NoError(t, err)

	store, err := sqlitevec.New[int64, int64](context.Background(), db, sqlitevec.Schema{
		DocsTable:      "messages",
		IDColumn:       "id",
		ContentColumn:  "body",
		EmbedGenColumn: "embed_gen",
		VectorsPrefix:  "message_vectors",
	})
	require.NoError(t, err)
	return db, store
}

func TestStoreFillThenSearch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, 'a cat sat'), (2, 'a dog ran')`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	stats, err := vector.Fill(ctx, store, 1, topicEncoder(), vector.FillOptions[int64]{})
	require.NoError(err)
	assert.Equal(2, stats.Documents)

	pending, err := store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	assert.Empty(pending, "nothing pending once every document is stamped")

	enc := func(int64) vector.EncodeFunc { return topicEncoder() }
	hits, err := vector.Search(ctx, store, "a cat", enc, vector.SearchOptions{})
	require.NoError(err)
	require.NotEmpty(hits)
	assert.Equal(int64(1), hits[0].Doc, "the cat query ranks the cat document first")
}

func TestStoreReembeddingReplacesVectors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, 'a cat sat')`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	require.NoError(store.SaveVectors(ctx, 1, 1, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))
	require.NoError(store.SaveVectors(ctx, 1, 1, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{0, 1, 0}}}))

	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{0, 1, 0}, 10)
	require.NoError(err)
	require.Len(hits, 1, "re-embedding replaces the prior vector rather than duplicating it")
	assert.InDelta(1.0, hits[0].Score, 1e-6, "stored vector now matches the new query")
}

func TestStoreSearchUnionsLiveGenerations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, 'a cat'), (2, 'a dog')`)
	require.NoError(err)

	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "v1", Dimensions: 3}, sqlitevec.StateActive))
	_, err = vector.Fill(ctx, store, 1, topicEncoder(), vector.FillOptions[int64]{})
	require.NoError(err)

	// The building generation has covered only doc 1 so far.
	require.NoError(store.EnsureGeneration(ctx, 2, vector.Generation{Model: "v2", Dimensions: 3}, sqlitevec.StateBuilding))
	require.NoError(store.SaveVectors(ctx, 2, 1, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))

	gens, err := store.LiveGenerations(ctx)
	require.NoError(err)
	assert.Equal([]int64{2, 1}, gens, "building precedes active in preference order")

	enc := func(int64) vector.EncodeFunc { return topicEncoder() }
	hits, err := vector.Search(ctx, store, "a cat", enc, vector.SearchOptions{})
	require.NoError(err)

	found := map[int64]bool{}
	for _, h := range hits {
		found[h.Doc] = true
	}
	assert.True(found[1], "shared doc is searchable")
	assert.True(found[2], "active-only doc is not dropped mid-migration (union coverage)")
}

func TestStoreSaveVectorsRejectsMissingDocument(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	_, store := setup(t)

	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	err := store.SaveVectors(ctx, 1, 999, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}})
	require.Error(err, "saving vectors for a document not in the source table fails")

	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{1, 0, 0}, 10)
	require.NoError(err)
	assert.Empty(hits, "no orphan vectors are committed when the source row is missing")
}

// setupWithRevision mirrors setup but adds a last_modified revision column
// so SaveVectors stamps optimistically.
func setupWithRevision(t *testing.T) (*sql.DB, *sqlitevec.Store[int64, int64]) {
	t.Helper()
	db, err := openSQLiteTestDB(t, filepath.Join(t.TempDir(), "vec.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	_, err = db.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY, body TEXT, embed_gen INTEGER,
		last_modified INTEGER NOT NULL DEFAULT 0)`)
	require.NoError(t, err)

	store, err := sqlitevec.New[int64, int64](context.Background(), db, sqlitevec.Schema{
		DocsTable:      "messages",
		IDColumn:       "id",
		ContentColumn:  "body",
		EmbedGenColumn: "embed_gen",
		RevisionColumn: "last_modified",
		VectorsPrefix:  "message_vectors",
	})
	require.NoError(t, err)
	return db, store
}

func TestStoreStaleRevisionLeavesDocumentPending(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	db, store := setupWithRevision(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat sat', 1)`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	pending, err := store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	require.Len(pending, 1)

	// A concurrent edit bumps the revision between scan and save.
	_, err = db.ExecContext(ctx, `UPDATE messages SET last_modified = 2 WHERE id = 1`)
	require.NoError(err)

	err = store.SaveVectors(ctx, 1, pending[0].Doc, pending[0].Revision,
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}})
	require.ErrorIs(err, vector.ErrStale)

	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{1, 0, 0}, 10)
	require.NoError(err)
	assert.Empty(hits, "a stale save commits no vectors")

	pending, err = store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	require.Len(pending, 1, "the changed document stays pending")

	// A retry with the fresh revision succeeds and drains pending.
	require.NoError(store.SaveVectors(ctx, 1, pending[0].Doc, pending[0].Revision,
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))
	pending, err = store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	assert.Empty(pending)
}

func TestStoreFillWithRevisionColumn(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	db, store := setupWithRevision(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat', 3), (2, 'a dog', 4)`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	stats, err := vector.Fill(ctx, store, 1, topicEncoder(), vector.FillOptions[int64]{})
	require.NoError(err)
	assert.Equal(2, stats.Documents)

	pending, err := store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	assert.Empty(pending)
}

func TestStoreSaveVectorsRevisionRequiresColumn(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, 'a cat')`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	err = store.SaveVectors(ctx, 1, 1, int64(5),
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}})
	require.ErrorContains(err, "revision")
}

func TestStoreStampOnlySaveDropsDocumentFromPending(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, 'a cat')`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	require.NoError(store.SaveVectors(ctx, 1, 1, nil, nil), "an empty save stamps without vectors")

	pending, err := store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	assert.Empty(pending, "a stamp-only document stops being pending")

	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{1, 0, 0}, 10)
	require.NoError(err)
	assert.Empty(hits)
}

func TestNewRejectsUnsafeIdentifiers(t *testing.T) {
	_, err := sqlitevec.New[int64, int64](context.Background(), nil, sqlitevec.Schema{
		DocsTable: "messages; DROP TABLE messages",
	})
	require.Error(t, err)
}
