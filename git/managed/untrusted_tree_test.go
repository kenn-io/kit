package managedworktree

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/fslink"
	gitcmd "go.kenn.io/kit/git/cmd"
	gitenv "go.kenn.io/kit/git/env"
)

func TestSupportsUntrustedTreeCheckoutGitVersion(t *testing.T) {
	for _, test := range []struct {
		output string
		goos   string
		want   bool
	}{
		{output: "git version 2.41.0", goos: "linux"},
		{output: "git version 2.41.9", goos: "linux"},
		{output: "git version 2.42.0", goos: "linux", want: true},
		{output: "git version 2.45.2 (Apple Git-145)", goos: "darwin", want: true},
		{output: "git version 2.52.2.windows.4", goos: "linux"},
		{output: "git version 2.52.2.windows.4", goos: "windows"},
		{output: "git version 2.53.0", goos: "windows"},
		{output: "git version 2.53.0.windows.2", goos: "windows"},
		{output: "git version 2.53.0.windows.3-malformed", goos: "windows"},
		{output: "git version 2.53.0.windows.3", goos: "linux", want: true},
		{output: "git version 2.53.0.windows.3", goos: "windows", want: true},
		{output: "git version 2.53.1.windows.1", goos: "windows", want: true},
		{output: "git version 2.54.0.windows.1", goos: "windows", want: true},
		{output: "not git", goos: "linux"},
	} {
		assert.Equal(t, test.want,
			supportsUntrustedTreeCheckoutGitVersion(test.output, test.goos),
			test.output)
	}
}

func TestMaterializeUntrustedTreePinsWorktree(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	require.NoError(os.WriteFile(
		filepath.Join(repo, "payload"), []byte("imported"), 0o600,
	))
	lifecycleGit(t, repo, "add", "payload")
	lifecycleGit(t, repo, "commit", "-m", "add payload")

	worktree := filepath.Join(t.TempDir(), "worktree")
	lifecycleGit(t, repo, "worktree", "add", "--no-checkout", "-b", "import",
		worktree)
	external := t.TempDir()
	externalPayload := filepath.Join(external, "payload")
	require.NoError(os.WriteFile(externalPayload, []byte("preserve"), 0o600))
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(os.WriteFile(globalConfig, []byte(
		"[core]\n\tworktree = "+filepath.ToSlash(external)+"\n",
	), 0o600))
	env := append(gitenv.StripAll(os.Environ()),
		"GIT_CONFIG_GLOBAL="+globalConfig,
		"GIT_WORK_TREE="+external,
	)

	err := materializeUntrustedTree(t.Context(), worktree,
		untrustedTreeIsolation{runner: gitcmd.Runner{
			Env: env, StripEnv: false, NoSystemConfig: true,
		}})

	require.NoError(err)
	assert.FileExists(filepath.Join(worktree, "payload"))
	externalContents, err := os.ReadFile(externalPayload)
	require.NoError(err)
	assert.Equal("preserve", string(externalContents))
}

func TestDeferredCheckoutRejectsFutureTreeConfig(t *testing.T) {
	for _, source := range []string{"global", "system", "include", "conditional-include", "global-link", "include-link", "directory-link", "external", "external-link"} {
		t.Run(source, func(t *testing.T) {
			root := initLifecycleRepo(t)
			require.NoError(t, os.WriteFile(filepath.Join(root, ".gitconfig"), []byte("[diff \"deferred\"]\ncommand = echo executed > driver-ran\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("payload diff=deferred\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "payload"), []byte("original\n"), 0o600))
			lifecycleGit(t, root, "add", ".")
			lifecycleGit(t, root, "commit", "-m", "tree configuration")
			lifecycleGit(t, root, "branch", "imported")
			lifecycleGit(t, root, "reset", "--hard", "HEAD^")
			path := filepath.Join(t.TempDir(), "checkout")
			runner := lifecycleTestRunner(t)
			switch source {
			case "global":
				runner.Env = append(runner.Env, "GIT_CONFIG_GLOBAL=.gitconfig")
			case "system":
				runner.Env = append(runner.Env, "GIT_CONFIG_NOSYSTEM=0", "GIT_CONFIG_SYSTEM="+filepath.Join(path, ".gitconfig"))
			case "global-link", "include-link", "external-link":
				target := filepath.Join(path, ".gitconfig")
				if source == "external-link" {
					target = filepath.Join(t.TempDir(), "missing.gitconfig")
				}
				links := t.TempDir()
				if err := os.Symlink(target, filepath.Join(links, "target")); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("cannot create file symlink: %v", err)
					}
					require.NoError(t, err)
				}
				link := filepath.Join(links, "config")
				require.NoError(t, os.Symlink("target", link))
				if source == "include-link" {
					lifecycleGit(t, root, "config", "include.path", link)
				} else {
					runner.Env = append(runner.Env, "GIT_CONFIG_GLOBAL="+link)
				}
			case "directory-link":
				link := filepath.Join(t.TempDir(), "directory")
				_, err := fslink.LinkDir(path, link)
				require.NoError(t, err)
				runner.Env = append(runner.Env, "GIT_CONFIG_GLOBAL="+filepath.Join(link, ".gitconfig"))
			case "include", "conditional-include":
				key := "include.path"
				if source == "conditional-include" {
					key = "includeIf.onbranch:topic.path"
				}
				relative, err := filepath.Rel(filepath.Join(root, ".git"), filepath.Join(path, ".gitconfig"))
				require.NoError(t, err)
				lifecycleGit(t, root, "config", key, filepath.ToSlash(relative))
			}
			_, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Path: path, Branch: "topic", BaseRef: "imported", NoCheckout: true,
				Checkout: CheckoutIsolated, FailureCleanup: CleanupDeferred, Runner: runner,
			})
			if source != "external" && source != "external-link" {
				require.ErrorContains(t, err, "configuration inside merge request worktree")
				assert.NoDirExists(t, path)
				assert.False(t, branchExistsInRepo(t, root, "topic"))
				return
			}
			require.NoError(t, err)
			_, stderr, err := runner.Run(t.Context(), path, nil, "reset", "--hard", "HEAD")
			require.NoError(t, err, "%s", stderr)
			require.NoError(t, os.WriteFile(filepath.Join(path, "payload"), []byte("changed\n"), 0o600))
			out, stderr, err := runner.Run(t.Context(), path, nil, "diff", "--", "payload")
			require.NoError(t, err, "%s", stderr)
			assert.Contains(t, string(out), "+changed")
			assert.NoFileExists(t, filepath.Join(path, "driver-ran"))
		})
	}
}
