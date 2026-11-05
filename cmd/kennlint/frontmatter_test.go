package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gittest "go.kenn.io/kit/git/test"
)

const goodFrontmatter = "---\ntitle: A page\ndescription: A useful page.\nlast_edited: 2026-11-01\n---\n\n# Body\n"

func isolateFrontmatterGit(t *testing.T) {
	t.Helper()
	// The CLI honors the user's Git line-ending settings. Give these tests
	// synthetic homes so their diff behavior cannot inherit those settings.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
}

func TestFrontmatterValidation(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		problem string
	}{
		{"valid", goodFrontmatter, ""},
		{"CRLF", strings.ReplaceAll(goodFrontmatter, "\n", "\r\n"), ""},
		{"leap day", strings.ReplaceAll(goodFrontmatter, "2026-11-01", "2024-02-29"), ""},
		{"single quoted", strings.ReplaceAll(goodFrontmatter, "2026-11-01", "'2026-11-01'"), "bare YYYY-MM-DD"},
		{"double quoted", strings.ReplaceAll(goodFrontmatter, "2026-11-01", `"2026-11-01"`), "bare YYYY-MM-DD"},
		{"datetime", strings.ReplaceAll(goodFrontmatter, "2026-11-01", "2026-11-01T00:00:00Z"), "bare YYYY-MM-DD"},
		{"impossible date", strings.ReplaceAll(goodFrontmatter, "2026-11-01", "2026-02-29"), "bare YYYY-MM-DD"},
		{"short date", strings.ReplaceAll(goodFrontmatter, "2026-11-01", "2026-1-1"), "bare YYYY-MM-DD"},
		{"year zero", strings.ReplaceAll(goodFrontmatter, "2026-11-01", "0000-01-01"), "bare YYYY-MM-DD"},
		{"missing date", strings.ReplaceAll(goodFrontmatter, "last_edited: 2026-11-01\n", ""), "bare YYYY-MM-DD"},
		{"null date", strings.ReplaceAll(goodFrontmatter, "2026-11-01", "null"), "bare YYYY-MM-DD"},
		{"block date", strings.ReplaceAll(goodFrontmatter, "2026-11-01", "|-\n  2026-11-01"), "bare YYYY-MM-DD"},
		{"tagged date", strings.ReplaceAll(goodFrontmatter, "2026-11-01", "!!timestamp 2026-11-01"), "bare YYYY-MM-DD"},
		{"string tagged date", strings.ReplaceAll(goodFrontmatter, "2026-11-01", "!!str 2026-11-01"), "bare YYYY-MM-DD"},
		{"empty title", strings.ReplaceAll(goodFrontmatter, "title: A page", "title: '  '"), "title must be a non-empty string"},
		{"numeric title", strings.ReplaceAll(goodFrontmatter, "title: A page", "title: 123"), "title must be a non-empty string"},
		{"missing title", strings.ReplaceAll(goodFrontmatter, "title: A page\n", ""), "title must be a non-empty string"},
		{"missing description", strings.ReplaceAll(goodFrontmatter, "description: A useful page.\n", ""), "description must be a non-empty string"},
		{"empty file", "", "missing opening"},
		{"body only", "# Heading\n", "missing opening"},
		{"leading blank", "\n" + goodFrontmatter, "missing opening"},
		{"unterminated", "---\ntitle: Page\n", "missing closing"},
		{"empty mapping", "---\n---\n", "must be a YAML mapping"},
		{"sequence", "---\n- page\n---\n", "must be a YAML mapping"},
		{"scalar", "---\npage\n---\n", "must be a YAML mapping"},
		{"invalid YAML", "---\ntitle: [\n---\n", "invalid frontmatter YAML"},
		{"duplicate key", strings.ReplaceAll(goodFrontmatter, "title: A page", "title: A page\ntitle: Another"), "already defined"},
		{"second document", "---\ntitle: Page\n...\nother: value\n---\n", "single YAML mapping"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := parseFrontmatter([]byte(tt.src))
			if tt.problem == "" {
				require.NoError(t, err)
				assert.Empty(t, b.problems())
				return
			}
			if err != nil {
				assert.ErrorContains(t, err, tt.problem)
			} else {
				assert.Contains(t, strings.Join(b.problems(), "\n"), tt.problem)
			}
		})
	}
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
	n, err := runFrontmatter(t.Context(), nil, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 3, n, "a root SKILL.md exempts nothing and a lowercase skill.md is ordinary")
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
	repo.WriteFile("pkg/notes.md", "# No metadata\n")
	repo.Run("add", ".")
	repo.Run("commit", "-m", "add files")
	repo.Run("branch", "base")
	for _, name := range names {
		repo.WriteFile(name, "# Edited without metadata\n")
	}
	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-changed", "base"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "only the ordinary nested document is checked")
	assert.Equal(t, "pkg/notes.md: title must be a non-empty string\npkg/notes.md: description must be a non-empty string\n", stderr.String())
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
	repo.CommitFile("page.md", goodFrontmatter, "add page")
	repo.Run("branch", "base")
	repo.Checkout("-b", "topic")
	repo.CommitFile("page.md", goodFrontmatter+"More content.\n", "edit page")
	// The base branch advances independently. Compare with its merge base,
	// rather than the base tip, which now has a different date.
	repo.Checkout("base")
	repo.CommitFile("page.md", strings.ReplaceAll(goodFrontmatter, "2026-11-01", "2026-11-02"), "advance base")
	repo.Checkout("topic")
	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-changed", "base"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, "page.md: content changed without changing last_edited\n", stderr.String())

	repo.WriteFile("page.md", strings.ReplaceAll(goodFrontmatter, "2026-11-01", "2026-11-02")+"More content.\n")
	repo.WriteFile("new.md", goodFrontmatter)
	repo.Run("add", "new.md")
	stderr.Reset()
	n, err = runFrontmatter(t.Context(), []string{"-changed", "base"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, stderr.String())
	assert.Contains(t, stdout.String(), "checked 2 Markdown files")

	// Uncommitted and metadata-only edits count, too.
	repo.WriteFile("page.md", strings.ReplaceAll(goodFrontmatter, "A page", "New title"))
	n, err = runFrontmatter(t.Context(), []string{"-changed", "base", "page.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	repo.WriteFile("page.md", goodFrontmatter)
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
	repo.WriteFile("page.md", strings.ReplaceAll(goodFrontmatter, "\n", "\r\n"))
	n, err = runFrontmatter(t.Context(), []string{"-changed", "base", "page.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n, "checkout line endings must not count as content changes")

	require.NoError(t, os.Remove("page.md"))
	n, err = runFrontmatter(t.Context(), []string{"-changed", "base", "page.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n, "deleted files need no metadata")
}

func TestFrontmatterFixBeforeFirstCommit(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepo(t, gittest.Options{ResolvePath: true})
	t.Chdir(repo.Root)
	repo.WriteFile("page.md", "---\ntitle: A page\ndescription: A useful page.\n---\nBody\n")
	repo.Run("add", "page.md")
	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	src, err := os.ReadFile("page.md")
	require.NoError(t, err)
	assert.Contains(t, string(src), "last_edited: "+time.Now().UTC().Format(time.DateOnly))
}

func TestFrontmatterFix(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	withoutDate := strings.ReplaceAll(goodFrontmatter, "last_edited: 2026-11-01\n", "")
	repo.WriteFile("committed.md", withoutDate)
	repo.Run("add", "committed.md")
	cmd := repo.Runner.Command(t.Context(), repo.Root, "commit", "-m", "add missing date")
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_DATE=2024-06-15T12:00:00Z", "GIT_COMMITTER_DATE=2024-06-15T12:00:00Z")
	_, err := cmd.CombinedOutput()
	require.NoError(t, err)
	commitDate := repo.Run("log", "-1", "--format=%cs", "--", "committed.md")
	require.Equal(t, "2024-06-15", commitDate)
	for name, src := range map[string]string{
		"new.md":       withoutDate,
		"quoted.md":    strings.ReplaceAll(goodFrontmatter, "2026-11-01", "'2026-11-01' # keep comment"),
		"crlf.md":      strings.ReplaceAll(strings.ReplaceAll(goodFrontmatter, "2026-11-01", `"2026-11-01"`), "\n", "\r\n"),
		"missing.md":   "# Body only\n",
		"invalid.md":   strings.ReplaceAll(goodFrontmatter, "2026-11-01", "'2026-02-30'"),
		"timestamp.md": strings.ReplaceAll(goodFrontmatter, "2026-11-01", "2026-11-01T10:00:00Z"),
	} {
		repo.WriteFile(name, src)
		repo.Run("add", "--", name)
	}
	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 4, n) // Two text fields, an invalid date, and a timestamp.
	assert.Contains(t, stderr.String(), "missing.md: title must be a non-empty string")
	assert.Contains(t, stderr.String(), "missing.md: description must be a non-empty string")
	for _, tt := range []struct{ name, date, body string }{
		{"committed.md", commitDate, "\n# Body\n"},
		{"new.md", time.Now().UTC().Format(time.DateOnly), "\n# Body\n"},
		{"quoted.md", "2026-11-01", "\n# Body\n"},
		{"crlf.md", "2026-11-01", "\r\n# Body\r\n"},
		{"missing.md", time.Now().UTC().Format(time.DateOnly), "# Body only\n"},
	} {
		src, err := os.ReadFile(tt.name)
		require.NoError(t, err)
		b, err := parseFrontmatter(src)
		require.NoError(t, err)
		assert.Equal(t, tt.date, b.fields["last_edited"].Value)
		assert.Zero(t, b.fields["last_edited"].Style)
		assert.Equal(t, tt.body, string(b.body))
		// A second fix must leave the entire file byte-for-byte unchanged.
		_, err = runFrontmatter(t.Context(), []string{"-fix", tt.name}, &stdout, &stderr)
		require.NoError(t, err)
		again, err := os.ReadFile(tt.name)
		require.NoError(t, err)
		assert.Equal(t, src, again)
	}
	quoted, err := os.ReadFile("quoted.md")
	require.NoError(t, err)
	assert.Contains(t, string(quoted), "last_edited: 2026-11-01 # keep comment")
}

func TestFrontmatterFixWritesThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on Windows")
	}
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	withoutDate := strings.ReplaceAll(goodFrontmatter, "last_edited: 2026-11-01\n", "")
	repo.WriteFile("target.md", withoutDate)
	require.NoError(t, os.Symlink("target.md", "alias.md"))
	repo.Run("add", "target.md", "alias.md")

	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, stderr.String())
	info, err := os.Lstat("alias.md")
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the link stays a link")
	data, err := os.ReadFile("target.md")
	require.NoError(t, err)
	assert.Contains(t, string(data), "last_edited: ")
}

func TestFrontmatterFixChangedComparesFinalGitContent(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile(".gitattributes", "*.md text eol=lf\n")
	repo.WriteFile("document.md", goodFrontmatter)
	repo.Run("add", ".gitattributes", "document.md")
	repo.Run("commit", "-m", "add document with canonical date")
	repo.Run("branch", "base")

	quoted := strings.ReplaceAll(goodFrontmatter, "2026-11-01", `"2026-11-01"`)
	repo.WriteFile("document.md", strings.ReplaceAll(quoted, "\n", "\r\n"))

	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-changed", "base", "document.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, stderr.String())
	assert.Contains(t, stdout.String(), "1 fixed")
	repo.Run("diff", "--quiet", "base", "--", "document.md")
}
