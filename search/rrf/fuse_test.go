package rrf_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/rrf"
)

func TestFuseScoresRanksAndStableTies(t *testing.T) {
	const k = 60.0
	hits, err := rrf.Fuse(k, []rrf.Leg[string]{
		{Name: "lexical", Weight: 1, Keys: []string{"a", "b", "a"}},
		{Name: "vector", Weight: 2, Keys: []string{"b", "c"}},
	})
	require.NoError(t, err)
	require.Len(t, hits, 3)

	byKey := map[string]rrf.Hit[string]{}
	for _, hit := range hits {
		byKey[hit.Key] = hit
	}
	assert.InDelta(t, 1/(k+1), byKey["a"].Score, 1e-12)
	assert.Len(t, byKey["a"].Contributions, 1)
	assert.Equal(t, 1, byKey["a"].Contributions[0].Rank)

	assert.InDelta(t, 1/(k+2)+2/(k+1), byKey["b"].Score, 1e-12)
	require.Len(t, byKey["b"].Contributions, 2)
	assert.Equal(t, "lexical", byKey["b"].Contributions[0].Leg)
	assert.Equal(t, 2, byKey["b"].Contributions[0].Rank)
	assert.Equal(t, "vector", byKey["b"].Contributions[1].Leg)
	assert.Equal(t, 1, byKey["b"].Contributions[1].Rank)

	assert.Equal(t, []string{"b", "c", "a"}, []string{hits[0].Key, hits[1].Key, hits[2].Key})

	tied, err := rrf.Fuse(k, []rrf.Leg[string]{
		{Name: "left", Weight: 1, Keys: []string{"a"}},
		{Name: "right", Weight: 1, Keys: []string{"b"}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, []string{tied[0].Key, tied[1].Key})
	swapped, err := rrf.Fuse(k, []rrf.Leg[string]{
		{Name: "right", Weight: 1, Keys: []string{"b"}},
		{Name: "left", Weight: 1, Keys: []string{"a"}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"b", "a"}, []string{swapped[0].Key, swapped[1].Key})

	_, err = rrf.Fuse(0, []rrf.Leg[string]{{Name: "lexical", Weight: 1, Keys: []string{"a"}}})
	require.Error(t, err)
	_, err = rrf.Fuse(k, []rrf.Leg[string]{{Name: "lexical", Weight: 0, Keys: []string{"a"}}})
	require.Error(t, err)
	_, err = rrf.Fuse(k, []rrf.Leg[string]{
		{Name: "lexical", Weight: 1},
		{Name: "lexical", Weight: 1},
	})
	require.Error(t, err)
}

func TestFuseGroupsKeepsAlternatesUntilTheCallerFilters(t *testing.T) {
	hits, err := rrf.FuseGroups(60, []rrf.GroupLeg[string, string]{
		{Name: "lexical", Weight: 1, Groups: []rrf.Group[string, string]{
			{Key: "g1", Members: []string{"fresh", "stale"}},
			{Key: "g2", Members: []string{"only-lexical"}},
			{Key: "g1", Members: []string{"extra"}},
		}},
		{Name: "vector", Weight: 1, Groups: []rrf.Group[string, string]{
			{Key: "g1", Members: []string{"stale", "vector-only"}},
		}},
	})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, "g1", hits[0].Group)
	require.Len(t, hits[0].Contributions, 2, "a repeated group in one leg contributes once")
	assert.Equal(t, 1, hits[0].Contributions[0].Rank)
	assert.Equal(t, []rrf.Alternate[string]{
		{Member: "fresh", Leg: "lexical"},
		{Member: "stale", Leg: "lexical"},
		{Member: "extra", Leg: "lexical"},
		{Member: "stale", Leg: "vector"},
		{Member: "vector-only", Leg: "vector"},
	}, hits[0].Alternates)
	assert.Equal(t, "g2", hits[1].Group)
	assert.Len(t, hits[1].Contributions, 1)
}
