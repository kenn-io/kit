package managedworktree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsolatedWorktreeTrustedMergeDrivers(t *testing.T) {
	isolateLifecycleGitConfig(t)
	for _, test := range []struct {
		name        string
		trusted     []string
		repoDriver  string
		named       bool
		deferred    bool
		wantTrusted bool
	}{
		{name: "global driver blocked by default"},
		{name: "global driver explicitly allowed", trusted: []string{"Trusted"}, wantTrusted: true},
		{name: "names are case sensitive", trusted: []string{"trusted"}},
		{name: "repository selects unlisted driver", trusted: []string{"Trusted"}, repoDriver: "blocked"},
		{name: "explicit allowance includes repository selection", trusted: []string{"Trusted"}, repoDriver: "Trusted", wantTrusted: true},
		{name: "named isolated checkout", trusted: []string{"Trusted"}, named: true, wantTrusted: true},
		{name: "deferred import", trusted: []string{"Trusted"}, deferred: true, wantTrusted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			origin, clone := initOriginAndClone(t)
			attrs := "payload filter=Trusted diff=Trusted"
			if test.repoDriver != "" {
				attrs += " merge=" + test.repoDriver
			}
			require.NoError(t, os.WriteFile(filepath.Join(origin, ".gitattributes"), []byte(attrs+"\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(origin, "payload"), []byte("base\n"), 0o600))
			lifecycleGit(t, origin, "add", ".")
			lifecycleGit(t, origin, "commit", "-qm", "base")
			lifecycleGit(t, origin, "checkout", "-qb", "current")
			require.NoError(t, os.WriteFile(filepath.Join(origin, "payload"), []byte("current\n"), 0o600))
			lifecycleGit(t, origin, "commit", "-qam", "current")
			lifecycleGit(t, origin, "checkout", "-q", "main")
			require.NoError(t, os.WriteFile(filepath.Join(origin, "payload"), []byte("other\n"), 0o600))
			lifecycleGit(t, origin, "commit", "-qam", "other")
			lifecycleGit(t, clone, "fetch", "-q", "origin")

			// All global files belong to the fixture, never the user's config.
			globalAttrs := filepath.Join(t.TempDir(), "attributes")
			require.NoError(t, os.WriteFile(globalAttrs, []byte("* merge=Trusted\n"), 0o600))
			lifecycleGit(t, clone, "config", "--global", "core.attributesFile", globalAttrs)
			const trustedCommand = `printf 'trusted\n' > "%A"`
			lifecycleGit(t, clone, "config", "--global", "merge.Trusted.driver", trustedCommand)
			lifecycleGit(t, clone, "config", "--global", "merge.blocked.driver", `printf 'blocked\n' > "%A"`)
			for _, key := range []string{"filter.Trusted.clean", "filter.Trusted.smudge", "diff.Trusted.command", "diff.Trusted.textconv"} {
				lifecycleGit(t, clone, "config", key, "false")
			}
			lifecycleGit(t, clone, "config", "filter.Trusted.required", "true")

			path := filepath.Join(t.TempDir(), "worktree")
			if test.named {
				_, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
					ProjectRoot: clone, Branch: "imported", Path: path, BaseRef: "origin/current",
					Checkout: CheckoutIsolated, Runner: lifecycleTestRunner(t), TrustedMergeDrivers: test.trusted,
				})
				require.NoError(t, err)
			} else {
				_, err := CreateWorktreeFromMergeRequest(t.Context(), MergeRequestWorktreeOptions{
					ProjectRoot: clone, Branch: "imported", Path: path, Number: 99,
					HeadBranch: "current", HeadRepoCloneURL: origin, ProjectRepoIdentity: identityOfCloneURL(origin),
					Runner: lifecycleTestRunner(t), TrustedMergeDrivers: test.trusted, NoCheckout: test.deferred,
				})
				require.NoError(t, err)
			}
			if test.deferred {
				lifecycleGit(t, path, "reset", "--hard", "HEAD")
			}
			assert.Equal(t, "", worktreeOnlyConfig(t, path, "filter.Trusted.clean"))
			assert.Equal(t, "false", worktreeOnlyConfig(t, path, "filter.Trusted.required"))
			assert.Equal(t, safeExternalDiffCommand, worktreeOnlyConfig(t, path, "diff.Trusted.command"))
			assert.Equal(t, safeTextconvCommand, worktreeOnlyConfig(t, path, "diff.Trusted.textconv"))
			assert.Equal(t, expectedSafeMergeDriverCommand(t, path), worktreeOnlyConfig(t, path, "merge.blocked.driver"))
			assert.Equal(t, trustedCommand, worktreeConfig(t, clone, "merge.Trusted.driver"), "the existing checkout is unchanged")

			cmd := lifecycleGitCommand(t, path, "merge", "origin/main")
			out, mergeErr := cmd.CombinedOutput()
			contents, err := os.ReadFile(filepath.Join(path, "payload"))
			require.NoError(t, err)
			if test.wantTrusted {
				require.NoError(t, mergeErr, string(out))
				assert.Equal(t, "trusted\n", string(contents))
				assert.Empty(t, worktreeOnlyConfig(t, path, "merge.Trusted.driver"))
			} else {
				require.Error(t, mergeErr, string(out))
				assert.Contains(t, string(contents), "<<<<<<< current")
				assert.Contains(t, string(contents), "current\n")
				assert.Contains(t, string(contents), "other\n")
			}
		})
	}
}
