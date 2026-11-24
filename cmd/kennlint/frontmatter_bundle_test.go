package main

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gittest "go.kenn.io/kit/git/test"
)

func TestSkillChangedBundleResources(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const guide = "# Guide\n\nSome text\nwrapped here.\n"
	restamped := strings.Replace(skillHeader, "12:00:00Z", "12:01:00Z", 1)
	repo.WriteFile("skills/a/SKILL.md", skillHeader)
	repo.WriteFile("skills/a/references/guide.md", guide)
	repo.WriteFile("skills/a/scripts/run.sh", "echo hi\n")
	repo.WriteFile("skills/a/nested/SKILL.md", skillHeader)
	repo.WriteFile("skills/a/nested/references/n.md", guide)
	repo.WriteFile("skills/b/SKILL.md", skillHeader)
	repo.WriteFile("skills/b/references/x.md", guide)
	require.NoError(t, os.MkdirAll("skills/c", 0o755))
	symlinks := os.Symlink("../b/SKILL.md", "skills/c/SKILL.md") == nil
	if !symlinks {
		repo.WriteFile("skills/c/SKILL.md", skillHeader)
	}
	repo.WriteFile("skills/c/references/r.md", guide)
	repo.Run("add", ".")
	repo.Run("commit", "-m", "base bundles")
	repo.Run("branch", "base")

	edit := func(name, content string) func() { return func() { repo.WriteFile(name, content) } }
	for _, tc := range []struct {
		name     string
		change   func()
		resource string // Empty when the change needs no restamp.
	}{
		{"reference edit", edit("skills/a/references/guide.md", strings.Replace(guide, "Some", "Other", 1)), "skills/a/references/guide.md"},
		{"reference edit restamped", func() {
			repo.WriteFile("skills/a/references/guide.md", strings.Replace(guide, "Some", "Other", 1))
			repo.WriteFile("skills/a/SKILL.md", restamped)
		}, ""},
		{"reference reflow", edit("skills/a/references/guide.md", "# Guide\n\nSome text wrapped\nhere.\n"), ""},
		{"script edit", edit("skills/a/scripts/run.sh", "echo hi \n"), "skills/a/scripts/run.sh"},
		{"resource added", func() {
			repo.WriteFile("skills/a/assets/new.txt", "")
			repo.Run("add", "skills/a/assets/new.txt")
		}, "skills/a/assets/new.txt"},
		{"untracked file", edit("skills/a/assets/scratch.txt", "x"), ""},
		{"resource deleted", func() { require.NoError(t, os.Remove("skills/a/scripts/run.sh")) }, "skills/a/scripts/run.sh"},
		{"nested skill", func() {
			repo.WriteFile("skills/a/nested/references/n.md", "changed\n")
			repo.WriteFile("skills/a/nested/SKILL.md", strings.Replace(skillHeader, "Body", "New", 1))
		}, ""},
		{"unlisted skill", edit("skills/b/references/x.md", "changed\n"), ""},
		{"symlinked skill", edit("skills/c/references/r.md", "changed\n"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo.Run("reset", "--hard", "-q", "base")
			repo.Run("clean", "-fdq")
			tc.change()
			var out, errs bytes.Buffer
			n, err := runFrontmatter(t.Context(), []string{"-managed-skill", "skills/a/SKILL.md", "-changed", "base"}, &out, &errs)
			require.NoError(t, err)
			if tc.resource == "" {
				assert.Zero(t, n, errs.String())
				return
			}
			assert.Equal(t, 1, n)
			assert.Equal(t, "skills/a/SKILL.md: bundle file "+tc.resource+
				" changed without a later metadata.generated.at; set metadata.generated.at to the edit time and metadata.generated.by to the actual editor\n", errs.String())
		})
	}
	if !symlinks {
		t.Log("symlinks unavailable; the symlinked skill case ran as an unlisted skill")
	}
	if runtime.GOOS != "windows" {
		t.Run("mode only", func(t *testing.T) {
			repo.Run("reset", "--hard", "-q", "base")
			require.NoError(t, os.Chmod("skills/a/scripts/run.sh", 0o755))
			var out, errs bytes.Buffer
			n, err := runFrontmatter(t.Context(), []string{"-managed-skill", "skills/a/SKILL.md", "-changed", "base"}, &out, &errs)
			require.NoError(t, err)
			assert.Zero(t, n, errs.String())
		})
	}
}

func TestRootSkillChangedBundle(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("SKILL.md", skillHeader)
	repo.WriteFile("references/guide.md", "# Guide\n")
	repo.Run("add", ".")
	repo.Run("commit", "-m", "base root skill")
	repo.WriteFile("references/guide.md", "# Changed guide\n")
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-managed-skill", "SKILL.md", "-changed", "HEAD"}, &out, &errs)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, errs.String(), "SKILL.md: bundle file references/guide.md changed")
}
