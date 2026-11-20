package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// OKF v0.3 §11.2 rule 1: a null value, an empty string, and an empty list are
// the same as an absent key, for every key the comparison reads.
func TestOKFAbsentEquivalenceIsGeneric(t *testing.T) {
	cases := []struct {
		name, before, after string
		meaningful          bool
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
		{name: "extension value changes", before: "x-owner: a\n", after: "x-owner: b\n", meaningful: true},
		{name: "extension content kept", before: "x-owner: {a: null}\n", after: "x-owner: {}\n", meaningful: true},
		{name: "null list member kept", before: "tags: [null]\n", after: "tags: []\n", meaningful: true},
		{name: "source author appears", before: "sources: [{resource: /a.md}]\n", after: "sources: [{resource: /a.md, author: me}]\n", meaningful: true},
		{name: "scalar usage_window kept", before: "usage_window: ''\n", after: "usage_window: soon\n", meaningful: true},
		{name: "runtime appears", before: "runtime: ''\n", after: "runtime: dbt\n", meaningful: true},
	}
	parse := func(header string) *frontmatterBlock {
		b, err := parseFrontmatter([]byte(strings.Replace(goodOKF, "type: Guide\n", "type: Guide\n"+header, 1)))
		require.NoError(t, err)
		return b
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.meaningful, frontmatterMeaningful(parse(tc.before), parse(tc.after), false))
		})
	}
}
