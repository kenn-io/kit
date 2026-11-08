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
		{"invoices invoice", []string{"invoices", "invoice"}, ""},
		{"invoices, please", []string{"invoices"}, ""},
		{", refund?!", []string{"refund"}, ""},
		{"$ invoice +", []string{"invoice"}, ""},
		{`unclosed "quote`, []string{"unclosed", "quote"}, ""},
		{`"first phrase" "second phrase"`, nil, `"first phrase" AND "second phrase"`},
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
	for _, query := range []string{"", `""`, "$ +", `"?!"`, `"! ?"`, `"?!" invoice`, `"$" invoice`, "\"invoice\x00 rounding\""} {
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
		{"ad", "ads"},
		{"up", "ups", "upped", "upping"},
		{"go", "goes"},
		{"round", "rounds", "rounded", "rounding"},
		{"stop", "stops", "stopped", "stopping"},
		{"policy", "policies"},
		{"box", "boxes", "boxed", "boxing"},
		{"quiz", "quizzes", "quizzed", "quizzing"},
		{"quip", "quips", "quipped", "quipping"},
		{"fez", "fezzes"},
		{"topaz", "topazes"},
		{"try", "tries", "tried", "trying"},
		{"use", "uses", "used", "using"},
		{"cache", "caches", "caching"},
		{"cookie", "cookies"},
		{"add", "adds", "added", "adding"},
		{"queue", "queues", "queued", "queuing"},
		{"free", "freed", "freeing"},
		{"panic", "panics", "panicked", "panicking"},
		{"dye", "dyed", "dyeing"},
		{"bus", "buses", "bussed", "bussing"},
		{"die", "died", "dying"},
		{"agree", "agrees", "agreed", "agreeing"},
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
		{"red", "ring"}, {"bring", "bred"}, {"spring", "spre"}, {"caring", "car"}, {"filing", "filled"}, {"string", "str"}, {"https", "http"}, {"being", "bed"}, {"seed", "see"}, {"feed", "fee"},
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

func TestNaturalWordFamiliesStaySeparate(t *testing.T) {
	for _, tc := range []struct {
		family []string
		other  string
	}{
		{[]string{"use", "uses", "used", "using"}, "us"},
		{[]string{"news"}, "new"},
		{[]string{"add", "adds", "added", "adding"}, "ad"},
	} {
		t.Run(tc.family[0], func(t *testing.T) {
			db := ftsDB(t, append(tc.family, tc.other))
			for _, query := range tc.family {
				got, err := lexical.Literal().PrepareNatural(query)
				require.NoError(t, err)
				assert.ElementsMatch(t, tc.family, matchBodies(t, db, got.Match), query)
			}
			got, err := lexical.Literal().PrepareNatural(tc.other)
			require.NoError(t, err)
			assert.Equal(t, []string{tc.other}, matchBodies(t, db, got.Match))
		})
	}
}

func TestNaturalFTS(t *testing.T) {
	bodies := []string{"rounding of invoices", "invoice rounding decision", "the invoiced total was rounded", "connection reset by peer", "reset the connection", "ax", "axe", "axes"}
	db := ftsDB(t, bodies)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"invoice rounding decision", bodies[:3]},
		{"ax axes", bodies[5:]},
		{"axes ax", bodies[5:]},
		{`"rounding of invoices"`, bodies[:1]},
		{`"connection reset" retry`, bodies[3:4]},
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
	var calls []string
	chinese, err := lexical.Chinese("runtime", func(text string) ([]lexical.Token, error) {
		calls = append(calls, text)
		return []lexical.Token{{Text: "搜索"}, {Text: "引擎"}}, nil
	})
	require.NoError(t, err)
	got, err = chinese.PrepareNatural("搜索引擎 retry")
	require.NoError(t, err)
	assert.Equal(t, []string{"搜索引擎"}, calls)
	assert.Equal(t, `("搜索" AND "引擎")`, got.Words[0].Match)
	db := ftsDB(t, []string{"搜索 引擎", "引擎", "retry"})
	assert.ElementsMatch(t, []string{"搜索 引擎", "retry"}, matchBodies(t, db, got.Match))
	for _, query := range []string{`搜索引擎 かな`, `"搜索引擎"`} {
		_, err = chinese.PrepareNatural(query)
		require.NoError(t, err)
	}
	assert.Len(t, calls, 1)
	failure := errors.New("segment failed")
	chinese.Segment = func(string) ([]lexical.Token, error) { return nil, failure }
	_, err = chinese.PrepareNatural("搜索")
	require.ErrorIs(t, err, failure)
}
