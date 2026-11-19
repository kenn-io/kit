package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOKFSources(t *testing.T) {
	cases := []struct {
		name, sources, problem string
	}{
		{"omitted", "", ""},
		{"empty list", "sources: []\n", ""},
		{"empty string", "sources: ''\n", ""},
		{"resources", "sources:\n  - {id: web, resource: 'https://private.example/page'}\n  - {resource: '../missing.md'}\n  - {resource: '/not-yet-written.md'}\n  - {resource: 'all queries in project X', extra: {anything: true}}\n", ""},
		{"distinct IDs", "sources: [{id: a, resource: same}, {id: b, resource: same}]\n", ""},
		{"null", "sources: null\n", ""},
		{"mapping", "sources: {resource: page}\n", ""},
		{"scalar", "sources: page\n", "sources must be a sequence"},
		{"scalar entry", "sources: [page]\n", "sources[0] must be a mapping"},
		{"null entry", "sources: [null]\n", "sources[0] must be a mapping"},
		{"missing resource", "sources: [{}]\n", "sources[0].resource must be a non-empty string"},
		{"empty resource", "sources: [{resource: ' '}]\n", "sources[0].resource must be a non-empty string"},
		{"numeric resource", "sources: [{resource: 42}]\n", "sources[0].resource must be a non-empty string"},
		{"mapping resource", "sources: [{resource: {url: page}}]\n", "sources[0].resource must be a non-empty string"},
		{"empty ID", "sources: [{id: '', resource: page}]\n", "sources[0].id must be a non-empty string"},
		{"null ID", "sources: [{id: null, resource: page}]\n", "sources[0].id must be a non-empty string"},
		{"numeric ID", "sources: [{id: 1, resource: page}]\n", "sources[0].id must be a non-empty string"},
		{"duplicate ID", "sources: [{id: same, resource: a}, {id: same, resource: b}]\n", "sources[1].id must be unique within sources"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(goodOKF, "type: Guide\n", "type: Guide\n"+tc.sources, 1)
			b, err := parseFrontmatter([]byte(src))
			require.NoError(t, err)
			if tc.problem == "" {
				assert.Empty(t, b.okfProblems())
			} else {
				assert.Equal(t, []string{tc.problem}, b.okfProblems())
			}
		})
	}
}

func TestOKFSourcesDoNotInferCitationIntent(t *testing.T) {
	for _, sources := range []string{"", "sources: [{id: api, resource: 'references/api.md'}]\n"} {
		src := strings.Replace(goodOKF, "type: Guide\n", "type: Guide\n"+sources, 1)
		src += "\nClaim.[^api] Ordinary note.[^note] Unknown label.[^unknown]\n\n[^api]: API guide\n[^note]: An aside\n\n`[^inline]`\n\n```markdown\n[^code]\n```\n\n[Future concept](/not-yet-written.md)\n"
		b, err := parseFrontmatter([]byte(src))
		require.NoError(t, err)
		assert.Empty(t, b.okfProblems())
		body := string(b.body)
		changed, _ := b.fixOKF("page.md", nil, "", "")
		assert.False(t, changed)
		assert.Equal(t, body, string(b.body))
	}
}
