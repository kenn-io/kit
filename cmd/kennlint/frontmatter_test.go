package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gittest "go.kenn.io/kit/git/test"
)

func isolateFrontmatterGit(t *testing.T) {
	t.Helper()
	// The CLI honors the user's Git line-ending settings. Give these tests
	// synthetic homes so their diff behavior cannot inherit those settings.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
}

func TestFrontmatterSelection(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	for _, name := range []string{
		"README.md", "AGENTS.md", "CLAUDE.md", ".github/PULL_REQUEST_TEMPLATE.md", "testdata/page.md", "pkg/testdata/deep/page.md",
		"vendor/page.md", "node_modules/pkg/page.md", "generated/page.md", "docs/README.md", "docs/AGENTS.md", "docs/CLAUDE.md",
		"docs/releases/v0.37.1.md", "CHANGELOG.md", "CHANGELOG-2026.md", "docs/CHANGELOG.md", "docs/changelog-2026.md", "docs/page with spaces.md",
	} {
		repo.WriteFile(name, "# Missing metadata\n")
		repo.Run("add", "-f", "--", name)
	}
	repo.WriteFile("untracked.md", "# Untracked\n")
	repo.WriteFile(".gitignore", "ignored.md\n")
	repo.WriteFile("ignored.md", "# Ignored\n")
	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-exclude", "generated/**"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Empty(t, stdout.String())
	assert.Equal(t, "docs/page with spaces.md: missing opening --- frontmatter delimiter\n", stderr.String())

	stderr.Reset()
	n, err = runFrontmatter(t.Context(), []string{"docs/README.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n, "nested README.md is excluded even when selected explicitly")
	assert.Empty(t, stderr.String())

	t.Chdir(filepath.Join(repo.Root, "docs"))
	stderr.Reset()
	n, err = runFrontmatter(t.Context(), []string{"README.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, stderr.String())

	t.Chdir(repo.Root)
	for _, name := range []string{"README.md", "AGENTS.md", "CLAUDE.md", "docs/README.md", "docs/AGENTS.md", "docs/CLAUDE.md", "CHANGELOG.md", "docs/CHANGELOG.md", "docs/changelog-2026.md"} {
		stdout.Reset()
		stderr.Reset()
		n, err = runFrontmatter(t.Context(), []string{name}, &stdout, &stderr)
		require.NoError(t, err)
		assert.Zero(t, n, "%s is excluded even when selected explicitly", name)
		assert.Empty(t, stderr.String())
	}

	_, err = runFrontmatter(t.Context(), []string{"-exclude", "["}, &stdout, &stderr)
	require.ErrorContains(t, err, "invalid exclude")
	_, err = runFrontmatter(t.Context(), []string{t.TempDir()}, &stdout, &stderr)
	assert.ErrorContains(t, err, "outside the repository")
}

func TestFrontmatterSkipsSkillBundles(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	for _, name := range []string{
		"SKILL.md", "page.md", "docs/skill.md", "skills/unrelated/page.md",
		"skills/example-skill/SKILL.md", "skills/example-skill/references/api.md",
	} {
		repo.WriteFile(name, "# No metadata\n")
	}
	repo.Run("add", ".")

	var stdout, stderr bytes.Buffer
	_, err := runFrontmatter(t.Context(), nil, &stdout, &stderr)
	require.ErrorContains(t, err, "root SKILL.md")
	n, err := runFrontmatter(t.Context(), []string{"docs", "page.md", "skills"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 3, n, "explicit read-only selections still check ordinary documents, not nested bundles")
	assert.Equal(t, "docs/skill.md: missing opening --- frontmatter delimiter\n"+
		"page.md: missing opening --- frontmatter delimiter\n"+
		"skills/unrelated/page.md: missing opening --- frontmatter delimiter\n", stderr.String())
}

func TestFrontmatterGlob(t *testing.T) {
	for _, tt := range []struct {
		pattern, file string
		want          bool
	}{
		{"**/testdata/**", "testdata/a.md", true},
		{"**/testdata/**", "a/testdata/b/c.md", true},
		{"**/testdata/**", "a/testdatabase/c.md", false},
		{"docs/releases/**", "docs/releases/v0.8.0.md", true},
		{"docs/releases/**", "docs/releases/archive/v0.8.0.md", true},
		{"**/CHANGELOG*.md", "CHANGELOG.md", true},
		{"**/CHANGELOG*.md", "CHANGELOG-2026.md", true},
		{"**/CHANGELOG*.md", "docs/CHANGELOG.md", true},
		{"**/changelog*.md", "docs/changelog-2026.md", true},
		{"docs/*.md", "docs/a/b.md", false},
		{"docs/**/*.md", "docs/a.md", true},
		{"docs/**/*.md", "docs/a/b.md", true},
		{"docs/page?.md", "docs/page1.md", true},
		{"README.md", "docs/README.md", false},
		{"**/README.md", "README.md", true},
		{"**/README.md", "docs/deep/README.md", true},
	} {
		t.Run(tt.pattern+"/"+tt.file, func(t *testing.T) {
			assert.Equal(t, tt.want, frontmatterGlob(tt.pattern, tt.file))
		})
	}
}

func TestFrontmatterSkipsInstructionFilesAtEveryDepth(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	names := []string{"pkg/README.md", "pkg/AGENTS.md", "pkg/deep/CLAUDE.md"}
	for _, name := range names {
		repo.WriteFile(name, "# No metadata\n")
	}
	repo.WriteFile("pkg/notes.md", strings.Replace(goodOKF, "description: Useful\n", "", 1))
	repo.Run("add", ".")
	repo.Run("commit", "-m", "add files")
	repo.Run("branch", "base")
	for _, name := range names {
		repo.WriteFile(name, "# Edited without metadata\n")
	}
	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-changed", "base", "-type-rule", "**=Guide"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the ordinary nested document is checked")
	assert.Contains(t, stderr.String(), "pkg/notes.md: description must be a non-empty string")
	for _, name := range names {
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		assert.Equal(t, "# Edited without metadata\n", string(data), "%s is left untouched by -fix", name)
	}
}

func TestFrontmatterChanged(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.CommitFile("page.md", goodOKF, "add page")
	repo.Run("branch", "base")
	repo.Checkout("-b", "topic")
	repo.CommitFile("page.md", goodOKF+"More content.\n", "edit page")
	// The base branch advances independently. Compare with its merge base,
	// rather than the base tip, which now has a different date.
	repo.Checkout("base")
	repo.CommitFile("page.md", strings.ReplaceAll(goodOKF, "2026-11-17", "2026-11-18"), "advance base")
	repo.Checkout("topic")
	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-changed", "base"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, stderr.String(), "page.md: content changed without a later generated.at")

	repo.WriteFile("page.md", strings.ReplaceAll(goodOKF, "2026-11-17", "2026-11-18")+"More content.\n")
	repo.WriteFile("new.md", goodOKF)
	repo.Run("add", "new.md")
	stderr.Reset()
	n, err = runFrontmatter(t.Context(), []string{"-changed", "base"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, stderr.String())
	assert.Contains(t, stdout.String(), "checked 2 Markdown files")

	// Uncommitted and metadata-only edits count, too.
	repo.WriteFile("page.md", strings.ReplaceAll(goodOKF, "A page", "New title"))
	n, err = runFrontmatter(t.Context(), []string{"-changed", "base", "page.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	repo.WriteFile("page.md", goodOKF)
	n, err = runFrontmatter(t.Context(), []string{"-changed", "base", "page.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)

	// Unix reads the executable bit from the file; Windows reads it from the index.
	require.NoError(t, os.Chmod("page.md", 0o755))
	repo.Run("update-index", "--chmod=+x", "page.md")
	n, err = runFrontmatter(t.Context(), []string{"-changed", "base", "page.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n, "a mode-only change is not a content edit")
	require.NoError(t, os.Chmod("page.md", 0o644))
	repo.Run("update-index", "--chmod=-x", "page.md")

	_, err = runFrontmatter(t.Context(), []string{"-changed", "missing-ref"}, &stdout, &stderr)
	require.ErrorContains(t, err, "merge-base")

	repo.Config("core.autocrlf", "true")
	repo.WriteFile("page.md", strings.ReplaceAll(goodOKF, "\n", "\r\n"))
	n, err = runFrontmatter(t.Context(), []string{"-changed", "base", "page.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n, "checkout line endings must not count as content changes")

	require.NoError(t, os.Remove("page.md"))
	n, err = runFrontmatter(t.Context(), []string{"-changed", "base", "page.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n, "deleted files need no metadata")
}

func TestFrontmatterSelectionSkipsUnsearchableOrdinaryDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory search permissions")
	}
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("chosen.md", goodOKF)
	repo.WriteFile("protected/other.md", goodOKF)
	repo.WriteFile("skills/a/SKILL.md", skillHeader)
	repo.Run("add", ".")
	require.NoError(t, os.Chmod("protected", 0o600))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(filepath.Join(repo.Root, "protected"), 0o700)) })
	if _, err := os.Lstat("protected/other.md"); !os.IsPermission(err) {
		t.Skip("this process can search protected directories")
	}
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-managed-skill", "skills/a/SKILL.md", "chosen.md"}, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	assert.Contains(t, out.String(), "checked 1 Markdown file")
}
