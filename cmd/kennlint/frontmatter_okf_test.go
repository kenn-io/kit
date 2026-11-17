package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const goodOKF = "---\ntype: Guide\ntitle: A page\ndescription: Useful\ngenerated:\n  by: human:test\n  at: 2026-11-17T12:00:00Z\n---\n\n# A page\n\nText here.\n"

func TestOKFDocumentationGuide(t *testing.T) {
	data, err := os.ReadFile("../../docs/adopting-docs.md")
	require.NoError(t, err)
	b, err := parseFrontmatter(data)
	require.NoError(t, err)
	assert.Empty(t, b.okfProblems())
}
