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
		"README.md", ".github/PULL_REQUEST_TEMPLATE.md", "testdata/page.md", "pkg/testdata/deep/page.md",
		"vendor/page.md", "node_modules/pkg/page.md", "generated/page.md", "docs/README.md", "docs/page with spaces.md",
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
	assert.Equal(t, 2, n)
	assert.Empty(t, stdout.String())
	assert.Equal(t, "docs/README.md: missing opening --- frontmatter delimiter\ndocs/page with spaces.md: missing opening --- frontmatter delimiter\n", stderr.String())

	stderr.Reset()
	n, err = runFrontmatter(t.Context(), []string{"docs/README.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, "docs/README.md: missing opening --- frontmatter delimiter\n", stderr.String())

	t.Chdir(filepath.Join(repo.Root, "docs"))
	stderr.Reset()
	n, err = runFrontmatter(t.Context(), []string{"README.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, stderr.String(), "docs/README.md:")

	_, err = runFrontmatter(t.Context(), []string{"-exclude", "["}, &stdout, &stderr)
	require.ErrorContains(t, err, "invalid exclude")
	_, err = runFrontmatter(t.Context(), []string{t.TempDir()}, &stdout, &stderr)
	assert.ErrorContains(t, err, "outside the repository")
}

func TestFrontmatterGlob(t *testing.T) {
	for _, tt := range []struct {
		pattern, file string
		want          bool
	}{
		{"**/testdata/**", "testdata/a.md", true},
		{"**/testdata/**", "a/testdata/b/c.md", true},
		{"**/testdata/**", "a/testdatabase/c.md", false},
		{"docs/*.md", "docs/a/b.md", false},
		{"docs/**/*.md", "docs/a.md", true},
		{"docs/**/*.md", "docs/a/b.md", true},
		{"docs/page?.md", "docs/page1.md", true},
		{"README.md", "docs/README.md", false},
	} {
		t.Run(tt.pattern+"/"+tt.file, func(t *testing.T) {
			assert.Equal(t, tt.want, frontmatterGlob(tt.pattern, tt.file))
		})
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

func TestFrontmatterSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on Windows")
	}
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("AGENTS.md", goodFrontmatter)
	require.NoError(t, os.Symlink("AGENTS.md", "CLAUDE.md"))
	repo.Run("add", "AGENTS.md", "CLAUDE.md")
	repo.Run("commit", "-m", "add instructions and alias")
	repo.Run("branch", "base")
	repo.WriteFile("AGENTS.md", goodFrontmatter+"Updated.\n")
	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-changed", "base"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Contains(t, stderr.String(), "CLAUDE.md: content changed without changing last_edited")
	repo.WriteFile("AGENTS.md", strings.ReplaceAll(goodFrontmatter, "2026-11-01", "'2026-11-02'"))
	n, err = runFrontmatter(t.Context(), []string{"-fix", "-changed", "base"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	info, err := os.Lstat("CLAUDE.md")
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink)

	outside := filepath.Join(t.TempDir(), "outside.md")
	const outsideBody = "# Outside the repository\n"
	require.NoError(t, os.WriteFile(outside, []byte(outsideBody), 0o600))
	require.NoError(t, os.Symlink(outside, "outside.md"))
	repo.Run("add", "outside.md")
	stderr.Reset()
	n, err = runFrontmatter(t.Context(), []string{"-fix", "outside.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, stderr.String(), "outside.md: Markdown symlink target is outside the repository")
	untouched, err := os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, outsideBody, string(untouched))

	require.NoError(t, os.Symlink("missing-target", "dangling.md"))
	repo.Run("add", "dangling.md")
	_, err = runFrontmatter(t.Context(), []string{"dangling.md"}, &stdout, &stderr)
	assert.ErrorContains(t, err, "dangling.md")
}
