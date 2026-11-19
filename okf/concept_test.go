package okf_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/okf"
)

func TestConceptSources(t *testing.T) {
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
		{"scalar", "sources: page\n", "sources must be a sequence or a mapping"},
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
			src := "---\ntype: Custom\n" + tc.sources + "---\n"
			b, err := okf.Parse([]byte(src))
			require.NoError(t, err)
			if tc.problem == "" {
				assert.Empty(t, b.ConceptProblems())
			} else {
				assert.Equal(t, []string{tc.problem}, b.ConceptProblems())
			}
		})
	}
}
