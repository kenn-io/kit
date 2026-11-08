package lexical_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/lexical"
)

func TestPrepareNatural(t *testing.T) {
	got, err := lexical.Literal().PrepareNatural(`invoice rounding "exact   phrase" of`)
	require.NoError(t, err)
	assert.Equal(t, []string{"exact phrase"}, got.Phrases)
	assert.Equal(t, lexical.Literal().Identity, got.Identity)
	for _, tc := range []struct {
		query string
		words []string
		match string
	}{
		{"the of", []string{"the", "of"}, `"the" OR "of"`},
		{"café v2", []string{"café", "v2"}, `"café" OR "v2"`},
		{"The invoice", []string{"invoice"}, ""},
		{`"connection reset" the`, nil, `"connection reset"`},
		{"Invoice invoices", []string{"invoice", "invoices"}, ""},
		{"invoices, please", []string{"invoices"}, ""},
		{"$ invoice +", []string{"invoice"}, ""},
		{"⚠️ build failed", []string{"build", "failed"}, ""},
		{"cafe\u0301", []string{"cafe\u0301"}, ""},
		{`unclosed "quote`, []string{"unclosed", "quote"}, ""},
		{`"first phrase" "second phrase"`, nil, `"first phrase" AND "second phrase"`},
		{"“invoice rounding”", nil, `"invoice rounding"`},
		{"„invoice rounding“ retry", []string{"retry"}, `"invoice rounding"`},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got, err := lexical.Literal().PrepareNatural(tc.query)
			require.NoError(t, err)
			var words []string
			for _, word := range got.Words {
				words = append(words, word.Text)
			}
			assert.Equal(t, tc.words, words)
			if tc.match != "" {
				assert.Equal(t, tc.match, got.Match)
			}
		})
	}
	for _, query := range []string{`""`, "$ +", `"! ?"`, `"$" invoice`, "\"invoice\x00 rounding\"", `"👩‍💻" invoice`} {
		t.Run(query, func(t *testing.T) {
			got, err := lexical.Literal().PrepareNatural(query)
			require.Error(t, err)
			assert.Equal(t, lexical.Natural{}, got)
		})
	}
	_, err = (lexical.Analyzer{}).PrepareNatural("invoice")
	require.Error(t, err)
	calls := 0
	chinese, err := lexical.Chinese("runtime", func(string) ([]lexical.Token, error) {
		calls++
		return []lexical.Token{{Text: "搜索"}}, nil
	})
	require.NoError(t, err)
	got, err = chinese.PrepareNatural("搜索\x00 invoice")
	require.Error(t, err)
	assert.Equal(t, lexical.Natural{}, got)
	assert.Zero(t, calls)
}

func TestNaturalForms(t *testing.T) {
	for _, family := range [][]string{
		{"invoice", "invoices", "invoiced", "invoicing"},
		{"age", "ages", "aged", "aging", "ageing"},
		{"ad", "ads"},
		{"up", "ups", "upped", "upping"},
		{"go", "goes"},
		{"round", "rounds", "rounded", "rounding"},
		{"stop", "stops", "stopped", "stopping"},
		{"gym", "gyms", "gymmed", "gymming"},
		{"policy", "policies"},
		{"box", "boxes", "boxed", "boxing"},
		{"quip", "quips", "quipped", "quipping"},
		{"topaz", "topazes"},
		{"try", "tries", "tried", "trying"},
		{"use", "uses", "used", "using"},
		{"cache", "caches", "caching"},
		{"cookie", "cookies"},
		{"add", "adds", "added", "adding"},
		{"queue", "queues", "queued", "queuing", "queueing"},
		{"free", "freed", "freeing"},
		{"panic", "panics", "panicked", "panicking"},
		{"dye", "dyed", "dyeing"},
		{"die", "died", "dying"},
		{"deploy", "deploys", "deployed", "deploying"},
		{"show", "shows", "showed"},
		{"video", "videos"},
		{"sync", "synced", "syncing"},
		{"spec", "specs", "specced"},
		{"toe", "toes", "toed", "toeing"},
	} {
		t.Run(family[0], func(t *testing.T) {
			db := ftsDB(t, family)
			for _, word := range family {
				got, err := lexical.Literal().PrepareNatural(word)
				require.NoError(t, err)
				require.Len(t, got.Words, 1)
				assert.ElementsMatch(t, family, matchBodies(t, db, got.Match), word)
			}
		})
	}
	for _, tc := range []struct{ query, miss string }{
		{"spring", "spre"},
		{"caring", "car"},
		{"filing", "filled"},
		{"https", "http"},
		{"seed", "see"},
		{"specked", "spec"},
		{"picked", "pic"},
		{"feed", "fee"},
		{"uses", "us"},
		{"added", "ad"},
		{"news", "new"},
		{"lens", "len"},
		{"his", "hi"},
		{"has", "ha"},
		{"ties", "ty"},
		{"trie", "trying"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got, err := lexical.Literal().PrepareNatural(tc.query)
			require.NoError(t, err)
			assert.NotContains(t, got.Words[0].Forms, tc.miss)
			reverse, err := lexical.Literal().PrepareNatural(tc.miss)
			require.NoError(t, err)
			assert.NotContains(t, reverse.Words[0].Forms, tc.query)
		})
	}
}

func TestNaturalFTS(t *testing.T) {
	bodies := []string{"rounding of invoices", "invoice rounding decision", "the invoiced total was rounded"}
	db := ftsDB(t, bodies)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"invoice rounding decision", bodies},
		{`"rounding of invoices"`, bodies[:1]},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got, err := lexical.Literal().PrepareNatural(tc.query)
			require.NoError(t, err)
			assert.ElementsMatch(t, tc.want, matchBodies(t, db, got.Match))
		})
	}
	got, err := lexical.Literal().PrepareNatural("invoice rounding decision")
	require.NoError(t, err)
	counts := map[string]int{}
	for _, word := range got.Words {
		for _, body := range matchBodies(t, db, word.Match) {
			counts[body]++
		}
	}
	assert.Equal(t, 3, counts[bodies[1]])
	assert.Equal(t, 2, counts[bodies[0]])
}

func TestNaturalCJK(t *testing.T) {
	got, err := lexical.CharacterPhrase().PrepareNatural("SQLite 検索")
	require.NoError(t, err)
	require.Len(t, got.Words, 2)
	assert.Equal(t, `"検 索"`, got.Words[1].Match)
	tokens := map[string][]lexical.Token{
		"搜索引擎": {{Text: "搜索"}, {Text: "引擎"}},
		"T恤":   {{Text: "T恤"}},
		"t恤":   {{Text: "t"}, {Text: "恤"}},
		"B超":   {{Text: "B超"}},
		"b超":   {{Text: "b"}, {Text: "超"}},
	}
	var calls []string
	chinese, err := lexical.Chinese("runtime", func(text string) ([]lexical.Token, error) {
		calls = append(calls, text)
		return tokens[text], nil
	})
	require.NoError(t, err)
	for _, tc := range []struct {
		query  string
		bodies []string
		want   []string
		calls  []string
	}{
		{"搜索引擎 retry", []string{"搜索 引擎", "引擎", "retry"}, []string{"搜索 引擎", "retry"}, []string{"搜索引擎"}},
		{"T恤 B超 RETRY", []string{"T恤", "B超", "retried"}, []string{"T恤", "B超", "retried"}, []string{"T恤", "B超"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			calls = nil
			got, err := chinese.PrepareNatural(tc.query)
			require.NoError(t, err)
			assert.ElementsMatch(t, tc.want, matchBodies(t, ftsDB(t, tc.bodies), got.Match))
			assert.Equal(t, tc.calls, calls)
		})
	}
	calls = nil
	for _, tc := range []struct{ query, match string }{
		{`搜索引擎 かな`, `"搜索引擎" OR "かな"`},
		{`"搜索引擎"`, `"搜索引擎"`},
	} {
		got, err = chinese.PrepareNatural(tc.query)
		require.NoError(t, err)
		assert.Equal(t, tc.match, got.Match)
	}
	assert.Empty(t, calls)
	failure := errors.New("segment failed")
	chinese.Segment = func(string) ([]lexical.Token, error) { return nil, failure }
	_, err = chinese.PrepareNatural("搜索")
	require.ErrorIs(t, err, failure)
}
