package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gittest "go.kenn.io/kit/git/test"
	"go.kenn.io/kit/okf"
)

func TestOKFReservedFiles(t *testing.T) {
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

func TestOKFFixPreservesExtensions(t *testing.T) {
	src := "---\ndescription: Useful\nx-root: {nested: [one, true, 2], date: \"2026-11-17\"}\ngenerated:\n  x-audit: {actor: other, ids: [1, 2]}\nsources:\n  - resource: ../reference.md\n    x-proof: {key: [a, b]}\n---\n# Page\n"
	b, err := okf.Parse([]byte(src))
	require.NoError(t, err)
	var before map[string]any
	require.NoError(t, b.Metadata.Decode(&before))
	changed, _ := fixOKF(b, "page.md", frontmatterRules{{"**", "Guide"}}, "human:test", "")
	require.True(t, changed)
	data, err := b.Marshal()
	require.NoError(t, err)
	after, err := okf.Parse(data)
	require.NoError(t, err)
	var values map[string]any
	require.NoError(t, after.Metadata.Decode(&values))
	assert.Equal(t, before["x-root"], values["x-root"])
	assert.Equal(t, before["sources"], values["sources"])
	assert.Equal(t, before["generated"].(map[string]any)["x-audit"], values["generated"].(map[string]any)["x-audit"])
	assert.Equal(t, b.Body, after.Body)
	assert.Empty(t, profileProblems(after))
}

func TestOKFReviewMetadata(t *testing.T) {
	for _, source := range []string{"", "sources: null\n", "sources: ''\n", "sources: []\n", "status: null\n", "status: ''\n", "status: []\n"} {
		before, err := okf.Parse([]byte(goodOKF))
		require.NoError(t, err)
		after, err := okf.Parse([]byte(strings.Replace(goodOKF, "type: Guide\n", source+"type: Guide\n", 1)))
		require.NoError(t, err)
		changed, err := frontmatterMeaningful(before, after)
		require.NoError(t, err)
		assert.False(t, changed)
	}
	before, err := okf.Parse([]byte(strings.Replace(goodOKF, "type: Guide\n", "sources: {resource: /page.md}\ntype: Guide\n", 1)))
	require.NoError(t, err)
	after, err := okf.Parse([]byte(strings.Replace(goodOKF, "type: Guide\n", "sources: [{resource: /page.md}]\nverified: {by: 'human:reviewer', at: '2026-11-18T00:00:00Z'}\ntype: Guide\n", 1)))
	require.NoError(t, err)
	changed, err := frontmatterMeaningful(before, after)
	require.NoError(t, err)
	assert.False(t, changed, "source list shape and verification do not alter generated content")
}

func TestOKFAliases(t *testing.T) {
	src := "---\nx-title: &title Page\nx-provenance: &provenance {by: 'human:test', at: '2026-11-17T12:00:00Z'}\nx-source: &source {resource: /reference.md}\ntype: Guide\ntitle: *title\ndescription: Useful\ngenerated: *provenance\nsources: [*source]\n---\n# Page\n"
	b, err := okf.Parse([]byte(src))
	require.NoError(t, err)
	assert.Empty(t, profileProblems(b))
	src = "---\ntype: Guide\ntitle: Page\ndescription: Useful\nx-template: &template {x-extra: [original, true]}\ngenerated: *template\n---\n# Page\n"
	b, err = okf.Parse([]byte(src))
	require.NoError(t, err)
	var before map[string]any
	require.NoError(t, b.Metadata.Decode(&before))
	changed, _ := fixOKF(b, "page.md", nil, "human:test", "")
	require.True(t, changed)
	data, err := b.Marshal()
	require.NoError(t, err)
	after, err := okf.Parse(data)
	require.NoError(t, err)
	assert.Empty(t, profileProblems(after))
	var values map[string]any
	require.NoError(t, after.Metadata.Decode(&values))
	assert.Equal(t, before["x-template"], values["x-template"], "fixing an alias must not mutate its unknown extension anchor")
}

func TestOKFReservedChangedGate(t *testing.T) {
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

func TestOKFAnchoredExtensionFix(t *testing.T) {
	for _, header := range []string{
		"last_edited: &old 2026-11-17\nx-snapshot: *old\n",
		"generated: &generation {x-tag: [one, 2]}\nx-snapshot: *generation\n",
	} {
		t.Run(header, func(t *testing.T) {
			isolateFrontmatterGit(t)
			repo := gittest.NewRepoWithCommit(t)
			t.Chdir(repo.Root)
			src := "---\ntype: Guide\ntitle: Page\ndescription: Useful\n" + header + "---\n# Page\n"
			before, err := okf.Parse([]byte(src))
			require.NoError(t, err)
			var original map[string]any
			require.NoError(t, before.Metadata.Decode(&original))
			repo.WriteFile("page.md", src)
			repo.Run("add", "page.md")
			var out, errs bytes.Buffer
			n, err := runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test"}, &out, &errs)
			require.NoError(t, err)
			assert.Zero(t, n, errs.String())
			got, err := os.ReadFile("page.md")
			require.NoError(t, err)
			after, err := okf.Parse(got)
			require.NoError(t, err, "encoded metadata must remain readable")
			var values map[string]any
			require.NoError(t, after.Metadata.Decode(&values))
			assert.Equal(t, original["x-snapshot"], values["x-snapshot"])
			assert.Empty(t, profileProblems(after))
		})
	}
}

func TestOKFNativeReleaseNotes(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const src = "# Release notes\nNative format\n"
	files := []string{"docs/release-notes.md", "release-notes.md", "pkg/RELEASE-NOTES.md", "pkg/RELEASE_NOTES.md", "pkg/release_notes.md"}
	for _, file := range files {
		repo.WriteFile(file, src)
	}
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	for _, args := range [][]string{nil, {"-fix", "-by", "human:test", "-type-rule", "**=Guide"}} {
		n, err := runFrontmatter(t.Context(), args, &out, &errs)
		require.NoError(t, err)
		assert.Zero(t, n, errs.String())
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.Equal(t, src, string(data), file)
	}
}
