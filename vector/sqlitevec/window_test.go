package sqlitevec_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/search/sqlquery"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

func TestQueryGenerationWindowReportsRawProbePastFilteredRows(t *testing.T) {
	ctx := t.Context()
	db, store := setupWithRevision(t)
	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat sat', 1), (2, 'a cat slept', 1)`)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	_, err = vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE messages SET last_modified = 2 WHERE id = 1; DELETE FROM messages WHERE id = 2`)
	require.NoError(t, err)

	w, err := store.QueryGenerationWindow(ctx, 1, vector.Vector{1, 0, 0}, 1)
	require.NoError(t, err)
	assert.Empty(t, w.Hits, "stale and deleted candidates are filtered")
	assert.True(t, w.HasProbe, "the raw candidate beyond the boundary remains observable")
}

func TestBuildCandidateQueryComposesAndAppliesFilterBeforeResultLimit(t *testing.T) {
	ctx := t.Context()
	db, store := setupWithRevision(t)
	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat sat', 1), (2, 'a dog ran', 1)`)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	_, err = vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(t, err)

	// The lookup and the query share the caller's transaction.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = store.BuildCandidateQuery(ctx, tx, 99, vector.Vector{1, 0, 0}, sqlitevec.CandidateQuery{CandidateLimit: 1})
	require.ErrorContains(t, err, "generation 99 not ensured")
	q, err := store.BuildCandidateQuery(ctx, tx, 1, vector.Vector{1, 0, 0}, sqlitevec.CandidateQuery{
		CandidateLimit:  2,
		ExtraSourceCols: []sqlquery.Column{{Name: "body", As: "text"}},
		ResultLimit:     1,
		SourcePredicate: sqlquery.Predicate{SQL: "d.id = ?", Args: []any{int64(2)}},
	})
	require.NoError(t, err)
	// Compose the generated relation while preserving its binding order.
	q.SQL = "SELECT doc_key, chunk_index, revision, score, text FROM (" + q.SQL + ") AS matches ORDER BY score DESC, doc_key"
	narrow, err := store.BuildCandidateQuery(ctx, tx, 1, vector.Vector{1, 0, 0}, sqlitevec.CandidateQuery{
		CandidateLimit: 1, ResultLimit: 1,
		SourcePredicate: sqlquery.Predicate{SQL: "d.id = ?", Args: []any{int64(2)}},
	})
	require.NoError(t, err)
	type result struct {
		doc      int64
		chunk    int
		revision int64
		score    float64
		text     string
	}
	results, err := q.All(ctx, tx, func(rows *sql.Rows) (result, error) {
		var r result
		err := rows.Scan(&r.doc, &r.chunk, &r.revision, &r.score, &r.text)
		return r, err
	})
	require.NoError(t, err)
	require.Equal(t, []result{{doc: 2, chunk: 0, revision: 1, text: "a dog ran"}}, results)

	// The smaller raw window contains only the cat, which the predicate
	// excludes. It does not expand to fetch the eligible dog document.
	results, err = narrow.All(ctx, tx, func(rows *sql.Rows) (result, error) {
		var r result
		err := rows.Scan(&r.doc, &r.chunk, &r.revision, &r.score)
		return r, err
	})
	require.NoError(t, err)
	require.Empty(t, results)
}

func TestQueryGenerationWindowReturnsRevisionAndExhaustion(t *testing.T) {
	ctx := t.Context()
	db, store := setupWithRevision(t)
	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat sat', 1)`)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	_, err = vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(t, err)

	w, err := store.QueryGenerationWindow(ctx, 1, vector.Vector{1, 0, 0}, 1)
	require.NoError(t, err)
	require.Len(t, w.Hits, 1)
	require.Equal(t, int64(1), w.Hits[0].Doc)
	require.Equal(t, int64(1), w.Hits[0].Revision)
	require.False(t, w.HasProbe)
	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{1, 0, 0}, 1)
	require.NoError(t, err)
	require.Equal(t, w.Hits, hits, "ordinary and window queries share revision-bearing results")
}
