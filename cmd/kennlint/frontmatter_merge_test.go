package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gittest "go.kenn.io/kit/git/test"
	"go.kenn.io/kit/okf"
)

func TestFrontmatterMergedProvenance(t *testing.T) {
	const provenance = "{by: human:original, at: '2026-11-17'}"
	for _, tc := range []struct{ name, file, header string }{
		{"doc-root", "page.md", "x: &x {generated: " + provenance + "}\n<<: *x"},
		{"doc-nested", "page.md", "x: &x " + provenance + "\ngenerated: {<<: *x}"},
		{"doc-explicit", "page.md", "x: &x " + provenance + "\ngenerated: {by: human:explicit, <<: [*x, {at: invalid}]}"},
		{"skill-metadata", "SKILL.md", "x: &x {metadata: {type: Skill, status: stable, generated: " + provenance + "}}\n<<: *x"},
		{"skill-generated", "SKILL.md", "x: &x {generated: " + provenance + "}\nmetadata: {type: Skill, status: stable, <<: *x}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			src := "---\ntype: Guide\ntitle: Example\ndescription: Example\n" + tc.header + "\n---\n# Example\n"
			file := filepath.Join(root, tc.file)
			require.NoError(t, os.WriteFile(file, []byte(src), 0o600))
			for range 2 {
				problems, changed, err := checkFrontmatterFile(root, tc.file, true, nil, "human:new", "")
				require.NoError(t, err)
				assert.Empty(t, problems)
				assert.False(t, changed)
				got, err := os.ReadFile(file)
				require.NoError(t, err)
				assert.Equal(t, src, string(got))
			}
		})
	}
}

func TestMergedProvenanceChangedContent(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const file = "page.md"
	const base = "---\ntype: Guide\ntitle: Page\ndescription: Page\nx: &x {by: human:first, at: '2026-11-17'}\ngenerated: {<<: *x}\n---\nBody\n"
	repo.WriteFile(file, base)
	repo.Run("add", file)
	repo.Run("commit", "-m", "base")
	changed := strings.Replace(base, "Body", "Updated", 1)
	for _, tc := range []struct {
		contents string
		stale    bool
	}{
		{changed, true},
		{strings.Replace(changed, "{<<: *x}", "{by: human:second, at: '2026-11-18', <<: *x}", 1), false},
	} {
		repo.WriteFile(file, tc.contents)
		var out, errs bytes.Buffer
		n, err := runFrontmatter(t.Context(), []string{"-changed", "HEAD", "-fix", "-by", "human:new", file}, &out, &errs)
		require.NoError(t, err)
		if tc.stale {
			assert.Equal(t, 1, n)
			assert.Contains(t, errs.String(), "without a later generated.at")
		} else {
			assert.Zero(t, n, errs.String())
		}
		got, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.Equal(t, tc.contents, string(got))
	}
}

func TestMappingFieldMergePrecedence(t *testing.T) {
	src := "---\na: &a {by: first, at: first}\nb: &b {by: second, at: second}\nmerged: {<<: [*a, *b], by: null}\nquoted: {'<<': *a}\n---\n"
	b, err := okf.Parse([]byte(src))
	require.NoError(t, err)
	at := mappingField(b.Field("merged"), "at")
	require.NotNil(t, at)
	assert.Equal(t, "first", at.Value)
	by := mappingField(b.Field("merged"), "by")
	require.NotNil(t, by)
	assert.Equal(t, "!!null", by.ShortTag())
	assert.Nil(t, mappingField(b.Field("quoted"), "by"))
}
