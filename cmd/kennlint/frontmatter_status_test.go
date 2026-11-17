package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOKFStatus(t *testing.T) {
	cases := []struct {
		name, field string
		valid       bool
	}{
		{"omitted", "", true},
		{"stable", "status: stable\n", true},
		{"draft", "status: draft\n", true},
		{"deprecated", "status: deprecated\n", true},
		{"invalid", "status: retired\n", false},
		{"empty", "status: ''\n", false},
		{"number", "status: 123\n", false},
		{"mapping", "status: {value: draft}\n", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			b, err := parseFrontmatter([]byte(strings.Replace(goodOKF, "type: Guide\n", tt.field, 1)))
			require.NoError(t, err)
			changed, _ := b.fixOKF("page.md", frontmatterRules{{"**", "Guide"}}, "", "")
			require.True(t, changed, "fix fills the missing type")
			if tt.valid {
				assert.Empty(t, b.okfProblems())
			} else {
				assert.Contains(t, b.okfProblems(), "status must be one of stable, draft, or deprecated")
			}
			data, _, err := b.encode()
			require.NoError(t, err)
			assert.Contains(t, string(data), tt.field, "fix never rewrites status")
		})
	}
}
