package okf_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/okf"
)

func TestNormalizedMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		different           bool
	}{
		{name: "empty parameters", before: "parameters: []\n"},
		{name: "null parameters", before: "parameters: null\n"},
		{name: "null usage_window", before: "usage_window: null\n"},
		{name: "empty usage_window", before: "usage_window: {}\n"},
		{name: "null usage_window members", before: "usage_window: {from: null, to: ''}\n"},
		{name: "empty runtime", before: "runtime: ''\n"},
		{name: "null superseded_by", before: "superseded_by: ~\n"},
		{name: "empty extension", before: "x-owner: ''\n"},
		{name: "null source author", before: "sources: [{resource: /a.md}]\n", after: "sources: [{resource: /a.md, author: null}]\n"},
		{name: "empty source last_modified", before: "sources: [{resource: /a.md}]\n", after: "sources: [{resource: /a.md, last_modified: ''}]\n"},
		{name: "null source usage_window", before: "sources: [{resource: /a.md}]\n", after: "sources: [{resource: /a.md, usage_window: null}]\n"},
		{name: "null parameter member", before: "parameters: [{name: year, type: integer}]\n", after: "parameters: [{name: year, type: integer, required: null}]\n"},
		{name: "extension value changes", before: "x-owner: a\n", after: "x-owner: b\n", different: true},
		{name: "extension content kept", before: "x-owner: {a: null}\n", after: "x-owner: {}\n", different: true},
		{name: "null list member kept", before: "tags: [null]\n", after: "tags: []\n", different: true},
		{name: "source author appears", before: "sources: [{resource: /a.md}]\n", after: "sources: [{resource: /a.md, author: me}]\n", different: true},
		{name: "scalar usage_window kept", before: "usage_window: ''\n", after: "usage_window: soon\n", different: true},
		{name: "runtime appears", before: "runtime: ''\n", after: "runtime: dbt\n", different: true},
		{name: "absent empty tags", before: "tags: []\n"},
		{name: "absent null stale_after", before: "stale_after: null\n"},
		{name: "absent empty resource", before: "resource: ''\n"},
		{name: "scalar tags", before: "tags: [one]\n", after: "tags: one\n"},
		{name: "singleton parameters", before: "parameters: [{name: year, type: integer}]\n", after: "parameters: {name: year, type: integer}\n"},
		{name: "quoted source date", before: "sources: [{resource: /page.md, last_modified: 2026-11-17}]\n", after: "sources: [{resource: /page.md, last_modified: '2026-11-17'}]\n"},
		{name: "source equivalent offset", before: "sources: [{resource: /page.md, last_modified: '2026-11-17T12:00:00Z'}]\n", after: "sources: [{resource: /page.md, last_modified: '2026-11-17T13:00:00+01:00'}]\n"},
		{name: "quoted stale_after date", before: "stale_after: 2026-11-17\n", after: "stale_after: '2026-11-17'\n"},
		{name: "stale_after equivalent offset", before: "stale_after: '2026-11-17T12:00:00Z'\n", after: "stale_after: '2026-11-17T13:00:00+01:00'\n"},
		{name: "usage window timestamps", before: "usage_window: {from: 2026-11-17, to: '2026-11-18T00:00:00Z'}\n", after: "usage_window: {from: '2026-11-17', to: '2026-11-18T01:00:00+01:00'}\n"},
		{name: "source timestamp changes", before: "sources: [{resource: /page.md, last_modified: '2026-11-17'}]\n", after: "sources: [{resource: /page.md, last_modified: '2026-11-18'}]\n", different: true},
		{name: "stale_after changes", before: "stale_after: '2026-11-17'\n", after: "stale_after: '2026-11-18'\n", different: true},
		{name: "singleton verification", before: "verified: {by: agent, at: 2026-11-17}\n", after: "verified: [{by: agent, at: '2026-11-17T01:00:00+01:00'}]\n"},
		{name: "generation date", before: "generated: {at: 2026-11-17}\n", after: "generated: {at: '2026-11-17'}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			normalize := func(header string) map[string]any {
				t.Helper()
				doc, err := okf.Parse([]byte("---\ntype: Page\n" + header + "---\n"))
				require.NoError(t, err)
				values, err := doc.NormalizedMetadata()
				require.NoError(t, err)
				return values
			}
			before, after := normalize(tc.before), normalize(tc.after)
			if tc.different {
				assert.NotEqual(t, before, after)
			} else {
				assert.Equal(t, before, after)
			}
		})
	}
}
