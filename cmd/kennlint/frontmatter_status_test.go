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
			src := strings.Replace(goodOKF, "type: Guide\n", "type: Guide\n"+tt.field, 1)
			b, err := parseFrontmatter([]byte(src))
			require.NoError(t, err)
			if tt.valid {
				assert.Empty(t, b.okfProblems())
			} else {
				assert.Contains(t, b.okfProblems(), "status must be one of stable, draft, or deprecated")
			}
			changed, _ := b.fixOKF("page.md", nil, "", "")
			assert.False(t, changed)
		})
	}
}

func TestOKFStatusFixAndChanged(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("page.md", goodOKF)
	repo.Run("add", "page.md")
	repo.Run("commit", "-m", "base")
	repo.Run("branch", "base")
	for _, status := range []string{"stable", "draft", "deprecated", "retired"} {
		t.Run(status, func(t *testing.T) {
			src := strings.Replace(goodOKF, "type: Guide\n", "status: "+status+"\n", 1)
			repo.WriteFile("page.md", src)
			var out, errout bytes.Buffer
			n, err := runFrontmatter(t.Context(), []string{"-fix", "-type-rule", "**=Guide", "page.md"}, &out, &errout)
			require.NoError(t, err)
			if status == "retired" {
				assert.Positive(t, n)
				assert.Contains(t, errout.String(), "status must be one of")
			} else {
				assert.Zero(t, n, errout.String())
			}
			data, err := os.ReadFile("page.md")
			require.NoError(t, err)
			assert.Contains(t, string(data), "status: "+status+"\n")
		})
	}
	repo.WriteFile("page.md", goodOKF)
	var out, errout bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "page.md"}, &out, &errout)
	require.NoError(t, err)
	assert.Zero(t, n)
	data, err := os.ReadFile("page.md")
	require.NoError(t, err)
	assert.Equal(t, goodOKF, string(data))
	assert.NotContains(t, string(data), "status:")
	repo.WriteFile("page.md", strings.Replace(goodOKF, "type: Guide\n", "type: Guide\nstatus: deprecated\n", 1))
	out.Reset()
	errout.Reset()
	n, err = runFrontmatter(t.Context(), []string{"-changed", "base", "page.md"}, &out, &errout)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, errout.String(), "content changed without a later generated.at")
}
