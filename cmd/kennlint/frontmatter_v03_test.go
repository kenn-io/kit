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
	for _, value := range []string{"2026-11-17", "'2026-11-17'", "2026-11-17T13:00:00+01:00", "\"2026-11-17t12:00:00z\"", "\"2026-11-17T07:00:00-05:00\""} {
		t.Run(value, func(t *testing.T) {
			b, err := parseFrontmatter([]byte(strings.Replace(goodOKF, "2026-11-17T12:00:00Z", value, 1)))
			require.NoError(t, err)
			assert.Empty(t, b.okfProblems())
			got, ok := parseTimestamp(mappingField(b.fields["generated"], "at"))
			require.True(t, ok)
			want := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
			if strings.Contains(strings.ToUpper(value), "T") {
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

func TestOKFV03ReviewMetadata(t *testing.T) {
	for _, source := range []string{"", "sources: null\n", "sources: ''\n", "sources: []\n", "status: null\n", "status: ''\n", "status: []\n"} {
		before, err := parseFrontmatter([]byte(goodOKF))
		require.NoError(t, err)
		after, err := parseFrontmatter([]byte(strings.Replace(goodOKF, "type: Guide\n", source+"type: Guide\n", 1)))
		require.NoError(t, err)
		assert.False(t, frontmatterMeaningful(before, after))
	}
	before, err := parseFrontmatter([]byte(strings.Replace(goodOKF, "type: Guide\n", "sources: {resource: /page.md}\ntype: Guide\n", 1)))
	require.NoError(t, err)
	after, err := parseFrontmatter([]byte(strings.Replace(goodOKF, "type: Guide\n", "sources: [{resource: /page.md}]\nverified: {by: 'human:reviewer', at: '2026-11-18T00:00:00Z'}\ntype: Guide\n", 1)))
	require.NoError(t, err)
	assert.False(t, frontmatterMeaningful(before, after), "source list shape and verification do not alter generated content")
}

func TestOKFV03Aliases(t *testing.T) {
	src := "---\nx-title: &title Page\nx-provenance: &provenance {by: 'human:test', at: '2026-11-17T12:00:00Z'}\nx-source: &source {resource: /reference.md}\ntype: Guide\ntitle: *title\ndescription: Useful\ngenerated: *provenance\nsources: [*source]\n---\n# Page\n"
	b, err := parseFrontmatter([]byte(src))
	require.NoError(t, err)
	assert.Empty(t, b.okfProblems())
	src = "---\ntype: Guide\ntitle: Page\ndescription: Useful\nx-template: &template {x-extra: [original, true]}\ngenerated: *template\n---\n# Page\n"
	b, err = parseFrontmatter([]byte(src))
	require.NoError(t, err)
	var before map[string]any
	require.NoError(t, b.doc.Decode(&before))
	changed, _ := b.fixOKF("page.md", nil, "human:test", "")
	require.True(t, changed)
	data, _, err := b.encode()
	require.NoError(t, err)
	after, err := parseFrontmatter(data)
	require.NoError(t, err)
	assert.Empty(t, after.okfProblems())
	var values map[string]any
	require.NoError(t, after.doc.Decode(&values))
	assert.Equal(t, before["x-template"], values["x-template"], "fixing an alias must not mutate its unknown extension anchor")
}

func TestOKFV03ReservedChangedGate(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	src := "---\nokf_version: \"0.3\"\ngenerated: {by: human:test, at: '2026-11-17T12:00:00Z'}\n---\n# Index\n"
	repo.WriteFile("index.md", src)
	repo.WriteFile("page.md", goodOKF)
	repo.Run("add", ".")
	repo.Run("commit", "-m", "fixture")
	repo.WriteFile("index.md", src+"- [New page](page.md)\n")
	repo.WriteFile("page.md", strings.Replace(goodOKF, "type: Guide\n", "type: Guide\nverified: {by: 'human:reviewer', at: '2026-11-18T00:00:00Z'}\n", 1))
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-changed", "HEAD"}, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
}
