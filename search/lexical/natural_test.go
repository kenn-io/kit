package lexical_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/lexical"
)

func TestPrepareNatural(t *testing.T) {
	got, err := lexical.Literal().PrepareNatural(`invoice rounding "exact   phrase" of`)
	require.NoError(t, err)
	require.Len(t, got.Words, 2)
	assert.Equal(t, "invoice", got.Words[0].Text)
	assert.Equal(t, []string{"invoice", "invoices", "invoiced", "invoicing"}, got.Words[0].Forms)
	assert.Contains(t, got.Words[1].Forms, "rounded")
	assert.Equal(t, []string{"exact phrase"}, got.Phrases)
	assert.Equal(t, `"exact phrase"`, got.Match)
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
		{"Invoice invoices", []string{"invoice"}, ""},
		{"invoices invoice", []string{"invoices"}, ""},
		{"invoices, please", []string{"invoices"}, ""},
		{", refund?!", []string{"refund"}, ""},
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
	for _, query := range []string{"", "  ", `""`, `"  "`, "?"} {
		_, err := lexical.Literal().PrepareNatural(query)
		require.Error(t, err, query)
	}
	_, err = (lexical.Analyzer{}).PrepareNatural("invoice")
	require.Error(t, err)
	assert.True(t, lexical.IsStopWord("the"))
	assert.False(t, lexical.IsStopWord("invoice"))
}

func TestNaturalForms(t *testing.T) {
	for _, family := range [][]string{
		{"invoice", "invoices", "invoiced", "invoicing"},
		{"ad", "ads"},
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
		{"license", "licenses"},
		{"database", "databases"},
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
		{"play", "plays", "played"},
		{"key", "keys", "keyed"},
		{"delay", "delays", "delayed"},
		{"fix", "fixes", "fixed"},
		{"mix", "mixes", "mixed"},
		{"tax", "taxes", "taxed"},
		{"show", "shows", "showed"},
		{"repo", "repos"},
		{"todo", "todos"},
		{"demo", "demos"},
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
				assert.Equal(t, word, got.Words[0].Forms[0])
				for _, form := range family {
					assert.Contains(t, got.Words[0].Forms, form, word)
				}
				assert.ElementsMatch(t, family, matchBodies(t, db, got.Match), word)
			}
		})
	}
	for _, tc := range []struct{ query, miss string }{
		{"caring", "car"}, {"filing", "filled"}, {"string", "str"}, {"https", "http"}, {"being", "bed"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			db := ftsDB(t, []string{tc.query, tc.miss})
			got, err := lexical.Literal().PrepareNatural(tc.query)
			require.NoError(t, err)
			assert.Equal(t, []string{tc.query}, matchBodies(t, db, got.Match))
			reverse, err := lexical.Literal().PrepareNatural(tc.miss)
			require.NoError(t, err)
			assert.NotContains(t, reverse.Words[0].Forms, tc.query)
		})
	}
}

func TestNaturalFTS(t *testing.T) {
	bodies := []string{"rounding of invoices", "invoice rounding decision", "the invoiced total was rounded", "connection reset by peer", "reset the connection", "error-401 status 500 NOT", "the of", "thing"}
	db := ftsDB(t, bodies)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"invoice rounding decision", bodies[:3]},
		{"invoices,", bodies[:3]},
		{`"rounding of invoices"`, bodies[:1]},
		{`"connection reset" retry`, bodies[3:4]},
		{`error-401 status:500 NOT`, bodies[5:6]},
		{"the of", []string{bodies[0], bodies[2], bodies[4], bodies[6]}},
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
	db := ftsDB(t, []string{"検 索", "索 検", "搜索 引擎", "引擎"})
	got, err := lexical.CharacterPhrase().PrepareNatural("SQLite 検索")
	require.NoError(t, err)
	require.Len(t, got.Words, 2)
	assert.Equal(t, `"検 索"`, got.Words[1].Match)
	assert.Equal(t, []string{"検 索"}, matchBodies(t, db, got.Match))
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
	assert.Equal(t, []string{"搜索 引擎"}, matchBodies(t, db, got.Match))
	for _, query := range []string{`かな`, `搜索引擎 かな`, `"搜索引擎"`} {
		_, err = chinese.PrepareNatural(query)
		require.NoError(t, err)
	}
	assert.Len(t, calls, 1)
	failure := errors.New("segment failed")
	chinese.Segment = func(string) ([]lexical.Token, error) { return nil, failure }
	_, err = chinese.PrepareNatural("搜索")
	require.ErrorIs(t, err, failure)
}

func ftsDB(t *testing.T, bodies []string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `CREATE TABLE docs(id INTEGER PRIMARY KEY, body TEXT); CREATE VIRTUAL TABLE docs_fts USING fts5(body, tokenize='unicode61');`)
	require.NoError(t, err)
	for _, body := range bodies {
		_, err = db.ExecContext(t.Context(), `INSERT INTO docs(body) VALUES (?)`, body)
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `INSERT INTO docs_fts(body) VALUES (?)`, body)
		require.NoError(t, err)
	}
	return db
}
