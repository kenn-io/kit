package okf_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/okf"
)

func TestParseInvalid(t *testing.T) {
	for _, tc := range []struct{ name, src, problem string }{
		{"empty file", "", "missing opening"},
		{"body only", "# Heading\n", "missing opening"},
		{"leading blank", "\n---\ntype: Page\n---\n", "missing opening"},
		{"unterminated", "---\ntitle: Page\n", "missing closing"},
		{"empty mapping", "---\n---\n", "must be a YAML mapping"},
		{"sequence", "---\n- page\n---\n", "must be a YAML mapping"},
		{"scalar", "---\npage\n---\n", "must be a YAML mapping"},
		{"invalid YAML", "---\ntitle: [\n---\n", "invalid frontmatter YAML"},
		{"duplicate key", "---\ntitle: A page\ntitle: Another\n---\n", "already defined"},
		{"nested duplicate", "---\ngenerated: {by: a, by: b}\n---\n", "already defined"},
		{"second document", "---\ntitle: Page\n...\nother: value\n---\n", "single YAML mapping"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := okf.Parse([]byte(tc.src))
			require.ErrorContains(t, err, tc.problem)
		})
	}
}

func TestEditAliasedField(t *testing.T) {
	const body = "# Body\r\n---\n"
	doc, err := okf.Parse([]byte("---\r\ntype: Custom\r\nx-original: &name First\r\ntitle: *name\r\n---\r\n" + body))
	require.NoError(t, err)
	require.Empty(t, doc.ConceptProblems())
	require.NotNil(t, doc.Field("title"))
	doc.Field("title").Value = "Revised"
	data, err := doc.Marshal()
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(data, []byte("---\r\n")))
	updated, err := okf.Parse(data)
	require.NoError(t, err)
	assert.Equal(t, "First", updated.Field("x-original").Value)
	assert.Equal(t, "Revised", updated.Field("title").Value)
	assert.Equal(t, body, string(updated.Body))
}
