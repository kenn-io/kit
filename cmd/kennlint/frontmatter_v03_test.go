package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gittest "go.kenn.io/kit/git/test"
)

func TestOKFV03Timestamps(t *testing.T) {
	for _, value := range []string{"2026-11-17", "'2026-11-17'", "2026-11-17T13:00:00+01:00", "\"2026-11-17T07:00:00-05:00\""} {
		t.Run(value, func(t *testing.T) {
			b, err := parseFrontmatter([]byte(strings.Replace(goodOKF, "2026-11-17T12:00:00Z", value, 1)))
			require.NoError(t, err)
			assert.Empty(t, b.okfProblems())
			got, ok := parseTimestamp(mappingField(b.fields["generated"], "at"))
			require.True(t, ok)
			want := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
			if strings.Contains(value, "T") {
				assert.True(t, got.Equal(want))
			} else {
				assert.Equal(t, want.Add(-12*time.Hour), got)
			}
		})
	}
}

func TestOKFV03ReservedFiles(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	index := "---\nokf_version: \"0.3\"\ntitle: Bundle\ndescription: Bundle docs\nlicense: MIT\nx-custom: {nested: [one, true, 2]}\n---\n# Index\n"
	repo.WriteFile("index.md", index)
	repo.WriteFile("log.md", "# History\n")
	repo.Run("add", "index.md", "log.md")
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test"}, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	got, err := os.ReadFile("index.md")
	require.NoError(t, err)
	assert.Equal(t, index, string(got))
	repo.WriteFile("log.md", goodOKF)
	errs.Reset()
	n, err = runFrontmatter(t.Context(), []string{"-fix", "log.md"}, &out, &errs)
	require.NoError(t, err)
	assert.Equal(t, 1, n, errs.String())
	assert.Contains(t, errs.String(), "log.md must not have frontmatter")
	got, err = os.ReadFile("log.md")
	require.NoError(t, err)
	assert.Equal(t, goodOKF, string(got))
}

func TestOKFV03FixPreservesExtensions(t *testing.T) {
	src := "---\ndescription: Useful\nx-root: {nested: [one, true, 2], date: \"2026-11-17\"}\ngenerated:\n  x-audit: {actor: other, ids: [1, 2]}\nsources:\n  - resource: ../reference.md\n    x-proof: {key: [a, b]}\n---\n# Page\n"
	b, err := parseFrontmatter([]byte(src))
	require.NoError(t, err)
	var before map[string]any
	require.NoError(t, b.doc.Decode(&before))
	changed, _ := b.fixOKF("page.md", frontmatterRules{{"**", "Guide"}}, "human:test", "")
	require.True(t, changed)
	data, _, err := b.encode()
	require.NoError(t, err)
	after, err := parseFrontmatter(data)
	require.NoError(t, err)
	var values map[string]any
	require.NoError(t, after.doc.Decode(&values))
	assert.Equal(t, before["x-root"], values["x-root"])
	assert.Equal(t, before["sources"], values["sources"])
	assert.Equal(t, before["generated"].(map[string]any)["x-audit"], values["generated"].(map[string]any)["x-audit"])
	assert.Equal(t, b.body, after.body)
	assert.Empty(t, after.okfProblems())
}
