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
	if symlinks {
		require.NoError(t, os.MkdirAll("skills/a/imported", 0o755))
		require.NoError(t, os.Symlink("../../b/SKILL.md", "skills/a/imported/SKILL.md"))
		repo.WriteFile("skills/a/imported/references/r.md", guide)
	}
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
		{"unmanaged nested skill resource", edit("skills/a/nested/references/n.md", "changed\n"), "skills/a/nested/references/n.md"},
		{"unmanaged nested SKILL.md", edit("skills/a/nested/SKILL.md", strings.Replace(skillHeader, "Body", "New", 1)), "skills/a/nested/SKILL.md"},
		{"unmanaged nested skill restamped", func() {
			repo.WriteFile("skills/a/nested/references/n.md", "changed\n")
			repo.WriteFile("skills/a/nested/SKILL.md", strings.Replace(skillHeader, "Body", "New", 1))
			repo.WriteFile("skills/a/SKILL.md", restamped)
		}, ""},
		{"unlisted skill", edit("skills/b/references/x.md", "changed\n"), ""},
		{"symlinked skill", edit("skills/c/references/r.md", "changed\n"), ""},
		{"symlinked nested skill", func() {
			if symlinks {
				repo.WriteFile("skills/a/imported/references/r.md", "changed\n")
			}
		}, "skills/a/imported/references/r.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !symlinks && tc.name == "symlinked nested skill" {
				t.Skip("symlinks unavailable")
			}
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

func TestRootSkillChangedUnmanagedNestedSkill(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("SKILL.md", skillHeader)
	repo.WriteFile("templates/t/SKILL.md", skillHeader)
	repo.WriteFile("templates/t/references/guide.md", "# Guide\n")
	repo.Run("add", ".")
	repo.Run("commit", "-m", "base root skill")
	repo.Run("branch", "base")
	repo.WriteFile("templates/t/references/guide.md", "# Changed guide\n")
	args := []string{"-managed-skill", "SKILL.md", "-changed", "base"}

	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), args, &out, &errs)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, errs.String(), "SKILL.md: bundle file templates/t/references/guide.md changed")

	repo.WriteFile("SKILL.md", strings.Replace(skillHeader, "12:00:00Z", "12:01:00Z", 1))
	errs.Reset()
	n, err = runFrontmatter(t.Context(), args, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
}

// The command never lists a nested skill as managed, so ownership by a managed
// nested skill is checked on the helper that applies it.
func TestBundleChangeManagedNestedSkillOwnsItsFiles(t *testing.T) {
	changes := map[string]string{"a/nested/references/n.md": "A"}
	managed := map[string]struct{}{"a": {}, "a/nested": {}}
	resource, err := frontmatterBundleChange(t.Context(), t.TempDir(), "", "a", changes, managed)
	require.NoError(t, err)
	assert.Empty(t, resource, "the parent must not see files a managed nested skill owns")
	resource, err = frontmatterBundleChange(t.Context(), t.TempDir(), "", "a/nested", changes, managed)
	require.NoError(t, err)
	assert.Equal(t, "a/nested/references/n.md", resource)
}

func TestScopedSkillBundleChanges(t *testing.T) {
	isolateFrontmatterGit(t)
	for _, tc := range []struct {
		name, dir, shortSelection string
		deleted, stale            bool
	}{
		{"nested edit", "skills/a/", "nested/ref.md", false, true},
		{"staged deletion", "skills/a/", "nested/ref.md", true, true},
		{"directory selection", "skills/a/", "nested", true, true},
		{"root bundle", "", "nested/ref.md", false, true},
		{"unrelated resource", "skills/a/", "other.md", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := gittest.NewRepoWithCommit(t)
			t.Chdir(repo.Root)
			owner, resource := tc.dir+"SKILL.md", tc.dir+"nested/ref.md"
			ownerHeader := skillHeader
			if tc.name == "nested edit" {
				ownerHeader = strings.Replace(skillHeader, "  type: Skill\n", "", 1)
			}
			repo.WriteFile(owner, ownerHeader)
			repo.WriteFile(tc.dir+"nested/SKILL.md", skillHeader)
			repo.WriteFile(resource, "# Reference\n")
			repo.WriteFile(tc.dir+"other.md", "unchanged\n")
			repo.Run("add", ".")
			repo.Run("commit", "-m", "bundle base")
			if tc.deleted {
				repo.Run("rm", resource)
			} else {
				repo.WriteFile(resource, "# Edited reference\n")
			}
			args := []string{"-changed", "HEAD", "-fix", "-by", "human:other", "-managed-skill", owner, tc.dir + tc.shortSelection}
			var out, errs bytes.Buffer
			n, err := runFrontmatter(t.Context(), args, &out, &errs)
			require.NoError(t, err)
			if tc.stale {
				assert.Equal(t, 1, n)
				assert.Contains(t, errs.String(), owner+": bundle file "+resource+" changed without a later metadata.generated.at")
			} else {
				assert.Zero(t, n, errs.String())
			}
			got, err := os.ReadFile(owner)
			require.NoError(t, err)
			assert.Equal(t, ownerHeader, string(got), "an indirectly checked owner is never fixed")
		})
	}
}
