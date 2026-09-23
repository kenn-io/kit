package clickhouse_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/clickhouse"
)

func TestBuildTextUsesNativePredicatesWithoutARank(t *testing.T) {
	q, err := clickhouse.BuildText(clickhouse.TextRequest{
		Table: "docs", Key: "id", TextColumn: "body",
		Match: clickhouse.MatchToken, Text: "かなを探します。",
		RevisionColumn:  "revision",
		SourcePredicate: clickhouse.Predicate{SQL: "t.tenant = ?", Args: []any{"t1"}},
		ExtraCols:       []clickhouse.Column{{Name: "title", As: "title"}},
		Limit:           5,
	})
	require.NoError(t, err)
	assert.Contains(t, q.SQL, "hasToken(t.`body`, ?)")
	assert.NotContains(t, q.SQL, "asciiCJK")
	assert.NotContains(t, q.SQL, "bm25")
	assert.Contains(t, q.SQL, "t.`revision` AS revision")
	assert.Contains(t, q.SQL, "ORDER BY doc_key ASC LIMIT ?")
	assert.Equal(t, []any{"かなを探します。", "t1", 5}, q.Args)
	assert.Less(t, strings.Index(q.SQL, " WHERE "), strings.Index(q.SQL, " ORDER BY "))

	tokens, err := clickhouse.BuildText(clickhouse.TextRequest{
		Table: "docs", Key: "id", TextColumn: "body",
		Match: clickhouse.MatchAll, Tokens: []string{"か", "な"}, Limit: 3,
	})
	require.NoError(t, err)
	assert.Contains(t, tokens.SQL, "hasAllTokens(t.`body`, ?)")
	_, err = clickhouse.BuildText(clickhouse.TextRequest{
		Table: "docs", Key: "id", TextColumn: "body",
		Match: clickhouse.MatchToken, Tokens: []string{"か"}, Limit: 1,
	})
	require.Error(t, err)
	assert.Contains(t, clickhouse.TokenizersQuery().SQL, "system.tokenizers")

	substring, err := clickhouse.BuildText(clickhouse.TextRequest{
		Table: "docs", Key: "id", TextColumn: "body",
		Match: clickhouse.MatchSubstring, Text: "かな", Limit: 10,
	})
	require.NoError(t, err)
	assert.Contains(t, substring.SQL, "positionUTF8(t.`body`, ?) > 0")
	assert.Equal(t, []any{"かな", 10}, substring.Args)
	_, err = clickhouse.BuildText(clickhouse.TextRequest{
		Table: "docs", Key: "id", TextColumn: "body",
		Match: clickhouse.MatchSubstring, Tokens: []string{"か"}, Limit: 1,
	})
	require.Error(t, err)
}

func TestBuildVectorKeepsCandidateLimitAndProbeSetting(t *testing.T) {
	q, err := clickhouse.BuildVector(clickhouse.VectorRequest{
		Table: "docs", Key: "id", VectorColumn: "embedding",
		Distance: clickhouse.VectorCosine, Query: []float32{1, 0},
		RevisionColumn: "revision", CandidateLimit: 4, Approximate: true, Probes: 64,
		SourcePredicate: clickhouse.Predicate{SQL: "t.tenant = ?", Args: []any{"t1"}},
	})
	require.NoError(t, err)
	assert.Contains(t, q.SQL, "1 - (cosineDistance(t.`embedding`, ?)) AS score")
	assert.Contains(t, q.SQL, "ORDER BY cosineDistance(t.`embedding`, ?) ASC LIMIT ?")
	assert.Contains(t, q.SQL, "SETTINGS hnsw_candidate_list_size_for_search = 64")
	assert.Equal(t, []any{[]float32{1, 0}, "t1", []float32{1, 0}, 4}, q.Args)

	_, err = clickhouse.BuildVector(clickhouse.VectorRequest{
		Table: "docs", Key: "id", VectorColumn: "embedding",
		Query: []float32{1}, CandidateLimit: 1, Probes: 8,
	})
	require.Error(t, err)
}

func TestBuildTextRejectsEmptyTokenList(t *testing.T) {
	_, err := clickhouse.BuildText(clickhouse.TextRequest{
		Table: "docs", Key: "id", TextColumn: "body",
		Tokens: []string{}, Limit: 1,
	})
	require.Error(t, err)

	tokens, err := clickhouse.BuildText(clickhouse.TextRequest{
		Table: "docs", Key: "id", TextColumn: "body",
		Tokens: []string{"a"}, Limit: 1,
	})
	require.NoError(t, err)
	assert.Contains(t, tokens.SQL, "hasAllTokens(t.`body`, ?)")
	assert.Equal(t, []any{[]string{"a"}, 1}, tokens.Args)

	text, err := clickhouse.BuildText(clickhouse.TextRequest{
		Table: "docs", Key: "id", TextColumn: "body",
		Text: "a", Limit: 1,
	})
	require.NoError(t, err)
	assert.Contains(t, text.SQL, "hasAllTokens(t.`body`, ?)")
	assert.Equal(t, []any{"a", 1}, text.Args)
}
