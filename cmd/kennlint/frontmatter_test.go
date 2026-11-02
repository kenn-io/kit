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

	"go.kenn.io/kit/fslink"
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

func addFrontmatterSymlink(t *testing.T, repo *gittest.Repo, name, target string) {
	t.Helper()
	linkTarget := filepath.Join(t.TempDir(), "link-target")
	require.NoError(t, os.WriteFile(linkTarget, []byte(target), 0o600))
	object := repo.Run("hash-object", "-w", linkTarget)
	repo.Run("update-index", "--add", "--cacheinfo", "120000,"+object+","+name)
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

func TestSkillFrontmatterValidation(t *testing.T) {
	base := "name: example-skill\ndescription: An example skill.\n"
	maxName := strings.Repeat("a", 64)
	maxDescription := strings.Repeat("é", 1024)
	maxCompatibility := strings.Repeat("x", 500)
	tests := []struct {
		name    string
		fields  string
		file    string
		problem string
	}{
		{"minimal portable profile", base, "skills/example-skill/SKILL.md", ""},
		{"root SKILL.md needs a named directory", base, "SKILL.md", "must be inside a named skill directory"},
		{"maximum name length", "name: " + maxName + "\ndescription: Example skill.\n", "skills/" + maxName + "/SKILL.md", ""},
		{"maximum Unicode description length", "name: example-skill\ndescription: " + maxDescription + "\n", "skills/example-skill/SKILL.md", ""},
		{"optional fields and vendor metadata", base + "license: MIT\ncompatibility: " + maxCompatibility + "\nmetadata:\n  vendor.version: \"can be any custom string\"\n  last_edited: \"not a required date\"\nallowed-tools: Read Grep\n", "skills/example-skill/SKILL.md", ""},
		{"empty custom metadata key is still a string", base + "metadata:\n  \"\": value\n", "skills/example-skill/SKILL.md", ""},
		{"missing name", "description: An example skill.\n", "skills/example-skill/SKILL.md", "name must be a non-empty string"},
		{"missing description", "name: example-skill\n", "skills/example-skill/SKILL.md", "description must be a non-empty string"},
		{"empty name", "name: '  '\ndescription: An example skill.\n", "skills/example-skill/SKILL.md", "name must be a non-empty string"},
		{"non-string name", "name: 42\ndescription: An example skill.\n", "skills/example-skill/SKILL.md", "name must be a non-empty string"},
		{"uppercase name", "name: Example-skill\ndescription: An example skill.\n", "skills/example-skill/SKILL.md", "name must be 1-64 lowercase letters"},
		{"name starts with hyphen", "name: -example\ndescription: An example skill.\n", "skills/-example/SKILL.md", "name must be 1-64 lowercase letters"},
		{"name ends with hyphen", "name: example-\ndescription: An example skill.\n", "skills/example-/SKILL.md", "name must be 1-64 lowercase letters"},
		{"consecutive hyphens", "name: example--skill\ndescription: An example skill.\n", "skills/example--skill/SKILL.md", "name must be 1-64 lowercase letters"},
		{"name too long", "name: " + strings.Repeat("a", 65) + "\ndescription: An example skill.\n", "skills/" + strings.Repeat("a", 65) + "/SKILL.md", "name must be 1-64 lowercase letters"},
		{"name differs from directory", base, "skills/other-skill/SKILL.md", "name must match skill directory"},
		{"empty description", "name: example-skill\ndescription: '  '\n", "skills/example-skill/SKILL.md", "description must be a non-empty string"},
		{"non-string description", "name: example-skill\ndescription: false\n", "skills/example-skill/SKILL.md", "description must be a non-empty string"},
		{"description too long", "name: example-skill\ndescription: " + strings.Repeat("x", 1025) + "\n", "skills/example-skill/SKILL.md", "description must be at most 1024 characters"},
		{"empty license", base + "license: ''\n", "skills/example-skill/SKILL.md", "license must be a non-empty string"},
		{"non-string license", base + "license: false\n", "skills/example-skill/SKILL.md", "license must be a non-empty string"},
		{"compatibility too long", base + "compatibility: " + strings.Repeat("x", 501) + "\n", "skills/example-skill/SKILL.md", "compatibility must be at most 500 characters"},
		{"non-string compatibility", base + "compatibility: 42\n", "skills/example-skill/SKILL.md", "compatibility must be a non-empty string"},
		{"metadata is not a mapping", base + "metadata: vendor\n", "skills/example-skill/SKILL.md", "metadata must be a mapping of string keys to string values"},
		{"metadata key is not a string", base + "metadata:\n  7: vendor\n", "skills/example-skill/SKILL.md", "metadata keys must be strings"},
		{"metadata value is not a string", base + "metadata:\n  vendor: 42\n", "skills/example-skill/SKILL.md", "metadata values must be strings"},
		{"allowed-tools must use the portable string form", base + "allowed-tools: [Read, Grep]\n", "skills/example-skill/SKILL.md", "allowed-tools must be a space-separated string"},
		{"Claude Code-only field is not portable", base + "user-invocable: false\n", "skills/example-skill/SKILL.md", "is not part of portable Agent Skills frontmatter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := []byte("---\n" + tt.fields + "---\n# Instructions\n")
			b, err := parseFrontmatter(src)
			require.NoError(t, err)
			problems := skillFrontmatterProblems(b, tt.file)
			if tt.problem == "" {
				assert.Empty(t, problems)
				return
			}
			assert.Contains(t, strings.Join(problems, "\n"), tt.problem)
		})
	}
}

func FuzzValidSkillName(f *testing.F) {
	for _, seed := range []string{"example-skill", "a", strings.Repeat("a", 64), "-example", "example-", "example--skill", "Example", "résumé"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if validSkillName(name) {
			require.GreaterOrEqual(t, len(name), 1)
			require.LessOrEqual(t, len(name), 64)
			require.Regexp(t, `^[a-z0-9]+(-[a-z0-9]+)*$`, name)
		}
	})
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
	assert.Equal(t, 4, n)
	assert.Empty(t, stdout.String())
	assert.Equal(t, "docs/AGENTS.md: missing opening --- frontmatter delimiter\ndocs/CLAUDE.md: missing opening --- frontmatter delimiter\ndocs/README.md: missing opening --- frontmatter delimiter\ndocs/page with spaces.md: missing opening --- frontmatter delimiter\n", stderr.String())

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

	t.Chdir(repo.Root)
	for _, name := range []string{"README.md", "AGENTS.md", "CLAUDE.md", "CHANGELOG.md", "docs/CHANGELOG.md", "docs/changelog-2026.md"} {
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

func TestFrontmatterSkillBundleFiles(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("skills/example-skill/SKILL.md", "---\nname: example-skill\ndescription: A portable example skill.\n---\n# Instructions\n")
	repo.WriteFile("skills/example-skill/references/api.md", "# Reference without required frontmatter\n")
	repo.WriteFile("skills/example-skill/examples/usage.md", "# Example without required frontmatter\n")
	repo.WriteFile("skills/unrelated/page.md", "# Not a skill bundle\n")
	repo.Run("add", "skills/example-skill/SKILL.md", "skills/example-skill/references/api.md", "skills/example-skill/examples/usage.md", "skills/unrelated/page.md")

	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), nil, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, "skills/unrelated/page.md: missing opening --- frontmatter delimiter\n", stderr.String())

	stdout.Reset()
	stderr.Reset()
	n, err = runFrontmatter(t.Context(), []string{"skills/example-skill/references/api.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, "frontmatter: checked 0 Markdown files (0 fixed)\n", stdout.String())
	assert.Empty(t, stderr.String())

	stdout.Reset()
	stderr.Reset()
	n, err = runFrontmatter(t.Context(), []string{"skills/example-skill/SKILL.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, "frontmatter: checked 1 Markdown files (0 fixed)\n", stdout.String())
	assert.Empty(t, stderr.String())
}

func TestFrontmatterLowercaseSkillMarkdownIsOrdinary(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("docs/skill.md", "---\ntitle: Skill notes\ndescription: Ordinary Markdown with a lowercase filename.\nlast_edited: 2026-11-02\n---\n# Skill notes\n")
	repo.WriteFile("docs/child.md", "# Ordinary child without frontmatter\n")
	repo.Run("add", "docs/skill.md", "docs/child.md")

	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), nil, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, "docs/child.md: missing opening --- frontmatter delimiter\n", stderr.String())
}

func TestFrontmatterRootSkillDoesNotExemptRepositoryMarkdown(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("SKILL.md", "---\nname: example-skill\ndescription: A portable example skill.\n---\n# Instructions\n")
	repo.WriteFile("page.md", "# Ordinary repository document\n")
	repo.Run("add", "SKILL.md", "page.md")

	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), nil, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Contains(t, stderr.String(), "SKILL.md: SKILL.md must be inside a named skill directory")
	assert.Contains(t, stderr.String(), "page.md: missing opening --- frontmatter delimiter")
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
	repo.WriteFile("target.md", goodFrontmatter)
	require.NoError(t, os.Symlink("target.md", "alias.md"))
	repo.Run("add", "target.md", "alias.md")
	repo.Run("commit", "-m", "add file and alias")
	repo.Run("branch", "base")
	repo.WriteFile("target.md", goodFrontmatter+"Updated.\n")
	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-changed", "base"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Contains(t, stderr.String(), "alias.md: content changed without changing last_edited")
	repo.WriteFile("target.md", strings.ReplaceAll(goodFrontmatter, "2026-11-01", "'2026-11-02'"))
	stderr.Reset()
	n, err = runFrontmatter(t.Context(), []string{"-fix", "-changed", "base", "alias.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	info, err := os.Lstat("alias.md")
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink)
	fixed, err := os.ReadFile("target.md")
	require.NoError(t, err)
	assert.Equal(t, strings.ReplaceAll(goodFrontmatter, "2026-11-01", "2026-11-02"), string(fixed), "fixing the selected symlink updates its target")

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

func TestFrontmatterCoreSymlinksFalsePlaceholder(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.CommitFile("target.md", goodFrontmatter, "add target")
	addFrontmatterSymlink(t, repo, "alias.md", "target.md")
	repo.Run("commit", "-m", "add symlink alias")
	repo.Config("core.symlinks", "false")
	repo.WriteFile("alias.md", "target.md")

	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"alias.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, "frontmatter: checked 1 Markdown files (0 fixed)\n", stdout.String())
	assert.Empty(t, stderr.String())

	repo.WriteFile("target.md", strings.ReplaceAll(goodFrontmatter, "2026-11-01", "'2026-11-01'"))
	stdout.Reset()
	stderr.Reset()
	n, err = runFrontmatter(t.Context(), []string{"-fix", "alias.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, stderr.String())
	placeholder, err := os.ReadFile("alias.md")
	require.NoError(t, err)
	assert.Equal(t, "target.md", string(placeholder), "fixing an alias must preserve its symlink placeholder")
	fixed, err := os.ReadFile("target.md")
	require.NoError(t, err)
	assert.Equal(t, goodFrontmatter, string(fixed))
}

func TestFrontmatterTrackedSymlinkReplacedWithRegularFile(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.CommitFile("target.md", goodFrontmatter, "add target")
	addFrontmatterSymlink(t, repo, "alias.md", "target.md")
	repo.Run("commit", "-m", "add symlink alias")
	repo.Config("core.symlinks", "true")
	repo.WriteFile("alias.md", goodFrontmatter)

	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"alias.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, "frontmatter: checked 1 Markdown files (0 fixed)\n", stdout.String())
	assert.Empty(t, stderr.String())

	replaced := strings.ReplaceAll(goodFrontmatter, "2026-11-01", "'2026-11-01'")
	repo.WriteFile("alias.md", replaced)
	stdout.Reset()
	stderr.Reset()
	n, err = runFrontmatter(t.Context(), []string{"-fix", "alias.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, stderr.String())
	fixed, err := os.ReadFile("alias.md")
	require.NoError(t, err)
	assert.Equal(t, goodFrontmatter, string(fixed))
	target, err := os.ReadFile("target.md")
	require.NoError(t, err)
	assert.Equal(t, goodFrontmatter, string(target), "fixing a regular replacement must not modify the symlink's former target")
}

func TestFrontmatterChangedThroughRepairedDanglingSymlink(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	addFrontmatterSymlink(t, repo, "alias.md", "target.md")
	repo.Run("commit", "-m", "add dangling symlink alias")
	repo.Config("core.symlinks", "false")
	repo.WriteFile("alias.md", "target.md")
	repo.Run("branch", "base")
	repo.WriteFile("target.md", goodFrontmatter)

	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-changed", "base", "alias.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, "frontmatter: checked 1 Markdown files (0 fixed)\n", stdout.String())
	assert.Empty(t, stderr.String())
}

func TestFrontmatterChangedThroughAbsoluteSymlinkPlaceholder(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.CommitFile("target.md", goodFrontmatter, "add target")
	repo.CommitFile("links/keep.md", goodFrontmatter, "add links directory")

	internal := filepath.Join(repo.Root, "target.md")
	outside := filepath.Join(t.TempDir(), "outside.md")
	outsideBody := strings.ReplaceAll(goodFrontmatter, "2026-11-01", "'2026-11-01'")
	require.NoError(t, os.WriteFile(outside, []byte(outsideBody), 0o600))
	addFrontmatterSymlink(t, repo, "links/internal.md", internal)
	addFrontmatterSymlink(t, repo, "links/outside.md", outside)
	repo.Run("commit", "-m", "add absolute symlinks")
	repo.Config("core.symlinks", "false")
	repo.WriteFile("links/internal.md", internal)
	repo.WriteFile("links/outside.md", outside)
	repo.Run("branch", "base")

	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-changed", "base", "links/internal.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, stderr.String())

	tree, err := frontmatterTree(t.Context(), repo.Root, "base")
	require.NoError(t, err)
	_, _, err = frontmatterPrevious(t.Context(), repo.Root, "links/outside.md", tree)
	require.ErrorIs(t, err, errFrontmatterHistoricalTargetUnavailable)

	stdout.Reset()
	stderr.Reset()
	n, err = runFrontmatter(t.Context(), []string{"-fix", "links/outside.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, stderr.String(), "links/outside.md: Markdown symlink target is outside the repository")
	unchanged, err := os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, outsideBody, string(unchanged), "fixing an external alias must not write outside the repository")
}

func TestFrontmatterWindowsRootedTargetAnchoring(t *testing.T) {
	for _, target := range []string{`\repository\target.md`, "/repository/target.md"} {
		assert.True(t, frontmatterIsAbsoluteTarget(target, true))
		assert.True(t, frontmatterWindowsRootedWithoutVolume(target))
		assert.Equal(t, "C:/repository/target.md", frontmatterWindowsRootedTarget("C:", target))
	}
	assert.False(t, frontmatterWindowsRootedWithoutVolume(`\\server\share\target.md`))
	assert.Equal(t, `\\server\share\target.md`, frontmatterWindowsRootedTarget("C:", `\\server\share\target.md`))
	assert.Equal(t, `D:\outside\target.md`, frontmatterWindowsRootedTarget("C:", `D:\outside\target.md`))
	assert.Equal(t, "C:/repository/target.md", frontmatterNormalizeLinkTarget(`C:\repository\dir\..\target.md`, true))
	assert.Equal(t, "C:/repository/target.md", frontmatterNormalizeLinkTarget("C:/repository/dir/../../repository/target.md", true))
	assert.Equal(t, "//server/share/target.md", frontmatterNormalizeLinkTarget(`\\server\share\dir\..\target.md`, true))
}

func TestFrontmatterRepositoryTargetUsesCaseInsensitiveSemantics(t *testing.T) {
	root := t.TempDir()
	target := strings.ToUpper(root) + "/target.md"

	got, absolute, err := frontmatterRepositoryTargetWithSemantics(root, target, false, true)
	require.NoError(t, err)
	assert.True(t, absolute)
	assert.Equal(t, "target.md", got)

	_, _, err = frontmatterRepositoryTargetWithSemantics(root, target, false, false)
	assert.ErrorIs(t, err, errFrontmatterSymlinkOutside)
}

func TestFrontmatterWindowsParentComponentsNormalizeBeforeTraversal(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for _, dir := range []string{"links", "trees/primary", "detour/inner"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o755))
	}
	files := map[string]string{
		"links/alias.md":           "../switch/child/../doc.md",
		"switch":                   "trees/primary",
		"trees/primary/child":      "../../detour/inner",
		"trees/primary/doc.md":     goodFrontmatter,
		"detour/doc.md":            strings.ReplaceAll(goodFrontmatter, "A page", "A different page"),
		"detour/inner/present.txt": "directory marker\n",
	}
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o600))
	}
	indexModes := map[string]string{
		"links/alias.md":      "120000",
		"switch":              "120000",
		"trees/primary/child": "120000",
	}

	windowsPath, err := frontmatterWorkingPathWithSemantics(root, "links/alias.md", indexModes, true, true, false)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "trees/primary/doc.md"), windowsPath)

	unixPath, err := frontmatterWorkingPathWithSemantics(root, "links/alias.md", indexModes, true, false, false)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "detour/doc.md"), unixPath)
}

func TestFrontmatterPreviousWindowsParentComponentsNormalizeBeforeTraversal(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	primary := goodFrontmatter + "Primary destination.\n"
	detour := strings.ReplaceAll(goodFrontmatter, "A page", "A different page") + "Detour destination.\n"
	repo.CommitFile("trees/primary/doc.md", primary, "add primary destination")
	repo.CommitFile("detour/doc.md", detour, "add detour destination")
	repo.CommitFile("detour/inner/present.txt", "directory marker\n", "add detour directory marker")
	addFrontmatterSymlink(t, repo, "links/alias.md", "../switch/child/../doc.md")
	addFrontmatterSymlink(t, repo, "switch", "trees/primary")
	addFrontmatterSymlink(t, repo, "trees/primary/child", "../../detour/inner")
	repo.Run("commit", "-m", "add parent-component aliases")

	tree, err := frontmatterTree(t.Context(), repo.Root, "HEAD")
	require.NoError(t, err)
	previous, _, err := frontmatterPreviousWithSemantics(t.Context(), repo.Root, "links/alias.md", tree, true, false)
	require.NoError(t, err)
	assert.Equal(t, primary, string(previous))

	previous, _, err = frontmatterPreviousWithSemantics(t.Context(), repo.Root, "links/alias.md", tree, false, false)
	require.NoError(t, err)
	assert.Equal(t, detour, string(previous))
}

func TestFrontmatterWindowsInternalJunction(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("NTFS junctions require Windows")
	}
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.CommitFile("target/document.md", goodFrontmatter, "add junction target")
	junction := filepath.Join(repo.Root, "junction")
	require.NoError(t, fslink.CreateJunction(filepath.Join(repo.Root, "target"), junction))
	repo.WriteFile("alias.md", "junction/document.md")

	resolved, err := frontmatterWorkingPathWithSemantics(repo.Root, "alias.md", map[string]string{"alias.md": "120000"}, true, true, true)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(repo.Root, "target", "document.md"), resolved)
}

func TestFrontmatterWindowsJunctionToSymlinkPlaceholder(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("NTFS junctions require Windows")
	}
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	invalid := strings.ReplaceAll(goodFrontmatter, "2026-11-01", "'2026-11-01'")
	repo.CommitFile("target/document.md", invalid, "add junction target")
	addFrontmatterSymlink(t, repo, "target/link.md", "document.md")
	addFrontmatterSymlink(t, repo, "alias.md", "junction/link.md")
	repo.Run("commit", "-m", "add aliases through junction")
	repo.Config("core.symlinks", "false")
	repo.WriteFile("target/link.md", "document.md")
	repo.WriteFile("alias.md", "junction/link.md")
	require.NoError(t, fslink.CreateJunction(filepath.Join(repo.Root, "target"), filepath.Join(repo.Root, "junction")))

	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"alias.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, stderr.String(), "alias.md: last_edited")
	assert.Contains(t, stderr.String(), "bare YYYY-MM-DD")

	stdout.Reset()
	stderr.Reset()
	n, err = runFrontmatter(t.Context(), []string{"-fix", "alias.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, stderr.String())
	assert.Contains(t, stdout.String(), "1 fixed")

	for name, target := range map[string]string{
		"alias.md":       "junction/link.md",
		"target/link.md": "document.md",
	} {
		placeholder, err := os.ReadFile(name)
		require.NoError(t, err)
		assert.Equal(t, target, string(placeholder), "fixing through a junction must preserve the symlink placeholder")
	}
	document, err := os.ReadFile("target/document.md")
	require.NoError(t, err)
	assert.Equal(t, goodFrontmatter, string(document), "fixing through a junction must update the resolved Markdown target")
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

func TestFrontmatterWindowsRootedPlaceholderTargets(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows rooted paths require Windows semantics")
	}
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	invalid := strings.ReplaceAll(goodFrontmatter, "2026-11-01", "'2026-11-01'")
	repo.CommitFile("target.md", invalid, "add target")
	volume := filepath.VolumeName(repo.Root)
	rootedTarget := strings.TrimPrefix(filepath.ToSlash(filepath.Join(repo.Root, "target.md")), filepath.ToSlash(volume))
	targets := map[string]string{
		"backslash-alias.md": strings.ReplaceAll(rootedTarget, "/", `\`),
		"slash-alias.md":     rootedTarget,
	}
	for alias, target := range targets {
		addFrontmatterSymlink(t, repo, alias, target)
	}
	repo.Run("commit", "-m", "add volume-less rooted aliases")
	repo.Config("core.symlinks", "false")
	for alias, target := range targets {
		repo.WriteFile(alias, target)
	}

	for alias, target := range targets {
		var stdout, stderr bytes.Buffer
		n, err := runFrontmatter(t.Context(), []string{"-fix", alias}, &stdout, &stderr)
		require.NoError(t, err)
		assert.Zero(t, n)
		assert.Empty(t, stderr.String())
		placeholder, err := os.ReadFile(alias)
		require.NoError(t, err)
		assert.Equal(t, target, string(placeholder), "fixing an alias must preserve its placeholder")
	}
	target, err := os.ReadFile("target.md")
	require.NoError(t, err)
	assert.Equal(t, goodFrontmatter, string(target))
}

func TestFrontmatterChainedPlaceholderCaseInsensitiveIndexMode(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.CommitFile("document.md", goodFrontmatter, "add document")
	addFrontmatterSymlink(t, repo, "link.md", "document.md")
	addFrontmatterSymlink(t, repo, "outer.md", "LINK.md")
	repo.Run("commit", "-m", "add chained aliases")
	repo.WriteFile("outer.md", "LINK.md")
	repo.WriteFile("LINK.md", "document.md")

	indexModes, err := frontmatterIndexModes(t.Context(), repo.Root)
	require.NoError(t, err)
	resolved, err := frontmatterWorkingPathWithCaseSemantics(repo.Root, "outer.md", indexModes, true, true)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(repo.Root, "document.md"), resolved)
}

func TestFrontmatterChangedThroughCaseInsensitiveHistoricalAliases(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	original := goodFrontmatter + "Original content.\n"
	repo.CommitFile("document.txt", original, "add document")
	repo.CommitFile("docs/guide.txt", original, "add guide")
	addFrontmatterSymlink(t, repo, "file-alias.md", "DOCUMENT.TXT")
	addFrontmatterSymlink(t, repo, "directory-alias.md", "DOCS/GUIDE.TXT")
	repo.Run("commit", "-m", "add aliases")
	repo.Config("core.symlinks", "false")
	repo.Config("core.ignoreCase", "true")
	repo.WriteFile("file-alias.md", "DOCUMENT.TXT")
	repo.WriteFile("directory-alias.md", "DOCS/GUIDE.TXT")
	repo.Run("branch", "base")
	updated := goodFrontmatter + "Updated content.\n"
	for _, file := range []string{"document.txt", "DOCUMENT.TXT", "docs/guide.txt", "DOCS/GUIDE.TXT"} {
		repo.WriteFile(file, updated)
	}

	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-changed", "base", "file-alias.md", "directory-alias.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "file-alias.md: content changed without changing last_edited")
	assert.Contains(t, stderr.String(), "directory-alias.md: content changed without changing last_edited")
}

func TestFrontmatterChangedAfterRepairingHistoricalExternalSymlink(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.CommitFile("target.md", goodFrontmatter, "add internal target")

	outside := filepath.Join(t.TempDir(), "outside.md")
	require.NoError(t, os.WriteFile(outside, []byte(goodFrontmatter), 0o600))
	addFrontmatterSymlink(t, repo, "alias.md", outside)
	addFrontmatterSymlink(t, repo, "relative-alias.md", "../outside.md")
	repo.Run("commit", "-m", "add external aliases")
	repo.Config("core.symlinks", "false")
	repo.WriteFile("alias.md", outside)
	repo.WriteFile("relative-alias.md", "../outside.md")
	repo.Run("branch", "base")
	repo.WriteFile("alias.md", "target.md")
	repo.WriteFile("relative-alias.md", "target.md")

	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-changed", "base", "alias.md", "relative-alias.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, "frontmatter: checked 2 Markdown files (0 fixed)\n", stdout.String())
	assert.Empty(t, stderr.String())
}

func TestFrontmatterChangedAfterReplacingHistoricalRepositoryDirectorySymlink(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.Config("core.symlinks", "true")
	repo.CommitFile("target.md", goodFrontmatter, "add target")
	addFrontmatterSymlink(t, repo, "alias.md", ".")
	repo.Run("commit", "-m", "add repository-directory alias")
	repo.Run("branch", "base")
	repo.WriteFile("alias.md", goodFrontmatter)

	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-changed", "base", "alias.md"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, "frontmatter: checked 1 Markdown files (0 fixed)\n", stdout.String())
	assert.Empty(t, stderr.String())
}

func TestFrontmatterPreviousPreservesGitFailures(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	entry := frontmatterTreeEntry{mode: "120000", hash: "not-a-git-object"}
	_, _, err := frontmatterPrevious(t.Context(), repo.Root, "alias.md", map[string]frontmatterTreeEntry{
		"alias.md": entry,
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, errFrontmatterHistoricalTargetUnavailable)
}

func TestFrontmatterChangedThroughHistoricalDirectorySymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on Windows")
	}
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("docs/guide.md", goodFrontmatter+"Original guide.\n")
	repo.WriteFile("docs-v2/guide.md", goodFrontmatter+"Replacement guide.\n")
	require.NoError(t, os.Symlink("docs", "docs-alias"))
	require.NoError(t, os.Symlink("docs-alias/guide.md", "guide-link.md"))
	repo.Run("add", "docs/guide.md", "docs-v2/guide.md", "docs-alias", "guide-link.md")
	repo.Run("commit", "-m", "add directory symlink")
	repo.Run("branch", "base")
	require.NoError(t, os.Remove("docs-alias"))
	require.NoError(t, os.Symlink("docs-v2", "docs-alias"))

	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-changed", "base"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, "guide-link.md: content changed without changing last_edited\n", stderr.String())
}

func TestFrontmatterSymlinkParentResolutionPreservesOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on Windows")
	}
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("nested/guide.md", goodFrontmatter+"Original nested guide.\n")
	repo.WriteFile("nested/dir/marker.md", goodFrontmatter)
	repo.WriteFile("guide.md", strings.ReplaceAll(goodFrontmatter, "2026-11-01", "2026-10-31")+"Root guide.\n")
	require.NoError(t, os.Symlink("nested/dir", "dir-alias"))
	require.NoError(t, os.Symlink("dir-alias/../guide.md", "guide-link.md"))
	repo.Run("add", "nested/guide.md", "nested/dir/marker.md", "guide.md", "dir-alias", "guide-link.md")
	repo.Run("commit", "-m", "add ordered symlink path")
	repo.Run("branch", "base")
	repo.WriteFile("nested/guide.md", goodFrontmatter+"Updated nested guide.\n")

	var stdout, stderr bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-changed", "base"}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Contains(t, stderr.String(), "nested/guide.md: content changed without changing last_edited")
	assert.Contains(t, stderr.String(), "guide-link.md: content changed without changing last_edited")
}
