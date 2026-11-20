package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gittest "go.kenn.io/kit/git/test"
)

func TestOKFReview3ChangedEquivalence(t *testing.T) {
	cases := []struct {
		name, beforeHeader, afterHeader, beforeBody, afterBody string
		meaningful                                             bool
	}{
		{name: "absent empty tags", beforeHeader: "tags: []\n"},
		{name: "absent null stale_after", beforeHeader: "stale_after: null\n"},
		{name: "absent empty resource", beforeHeader: "resource: ''\n"},
		{name: "scalar tags", beforeHeader: "tags: [one]\n", afterHeader: "tags: one\n"},
		{name: "singleton parameters", beforeHeader: "parameters: [{name: year, type: integer}]\n", afterHeader: "parameters: {name: year, type: integer}\n"},
		{name: "quoted source date", beforeHeader: "sources: [{resource: /page.md, last_modified: 2026-11-17}]\n", afterHeader: "sources: [{resource: /page.md, last_modified: '2026-11-17'}]\n"},
		{name: "source equivalent offset", beforeHeader: "sources: [{resource: /page.md, last_modified: '2026-11-17T12:00:00Z'}]\n", afterHeader: "sources: [{resource: /page.md, last_modified: '2026-11-17T13:00:00+01:00'}]\n"},
		{name: "quoted stale_after date", beforeHeader: "stale_after: 2026-11-17\n", afterHeader: "stale_after: '2026-11-17'\n"},
		{name: "stale_after equivalent offset", beforeHeader: "stale_after: '2026-11-17T12:00:00Z'\n", afterHeader: "stale_after: '2026-11-17T13:00:00+01:00'\n"},
		{name: "usage window timestamps", beforeHeader: "usage_window: {from: 2026-11-17, to: '2026-11-18T00:00:00Z'}\n", afterHeader: "usage_window: {from: '2026-11-17', to: '2026-11-18T01:00:00+01:00'}\n"},
		{name: "source timestamp changes", beforeHeader: "sources: [{resource: /page.md, last_modified: '2026-11-17'}]\n", afterHeader: "sources: [{resource: /page.md, last_modified: '2026-11-18'}]\n", meaningful: true},
		{name: "stale_after changes", beforeHeader: "stale_after: '2026-11-17'\n", afterHeader: "stale_after: '2026-11-18'\n", meaningful: true},
		{name: "referenced footnote content", beforeBody: "\nClaim.[^a]\n\n[^a]: A\n", afterBody: "\nClaim.[^a]\n\n[^a]: <A>\n", meaningful: true},
		{name: "footnote definition placement", beforeBody: "\nClaim.[^a]\n\n[^a]: A\n", afterBody: "\n[^a]: A\n\nClaim.[^a]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateFrontmatterGit(t)
			repo := gittest.NewRepoWithCommit(t)
			t.Chdir(repo.Root)
			before := strings.Replace(goodOKF, "type: Guide\n", "type: Guide\n"+tc.beforeHeader, 1) + tc.beforeBody
			after := strings.Replace(goodOKF, "type: Guide\n", "type: Guide\n"+tc.afterHeader, 1) + tc.afterBody
			repo.WriteFile("page.md", before)
			repo.Run("add", "page.md")
			repo.Run("commit", "-m", "fixture")
			repo.WriteFile("page.md", after)
			var out, errs bytes.Buffer
			n, err := runFrontmatter(t.Context(), []string{"-changed", "HEAD", "page.md"}, &out, &errs)
			require.NoError(t, err)
			if tc.meaningful {
				assert.Equal(t, 1, n, errs.String())
				assert.Contains(t, errs.String(), "content changed without a later generated.at")
			} else {
				assert.Zero(t, n, errs.String())
			}
			got, err := os.ReadFile("page.md")
			require.NoError(t, err)
			assert.Equal(t, after, string(got), "the check must not rewrite metadata")
		})
	}
}

func TestOKFReview3RFC3339OffsetRanges(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"2026-11-17T12:00:00+24:00", false},
		{"2026-11-17T12:00:00-24:00", false},
		{"2026-11-17T12:00:00+01:60", false},
		{"2026-11-17T12:00:00-01:60", false},
		{"2026-11-17T12:00:00+23:59", true},
		{"2026-11-17T12:00:00-23:59", true},
		{"2026-11-17T12:00:00+00:59", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			src := strings.Replace(goodOKF, "2026-11-17T12:00:00Z", "'"+tc.value+"'", 1)
			b, err := parseFrontmatter([]byte(src))
			require.NoError(t, err)
			if tc.valid {
				assert.Empty(t, b.okfProblems())
			} else {
				assert.Contains(t, strings.Join(b.okfProblems(), "\n"), "generated.at must be")
			}
		})
	}
}
