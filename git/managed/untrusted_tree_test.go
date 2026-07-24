package managedworktree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitenv "go.kenn.io/kit/git/env"
)

func TestSupportsUntrustedTreeCheckoutGitVersion(t *testing.T) {
	for _, test := range []struct {
		output string
		goos   string
		want   bool
	}{
		{output: "git version 2.39.0", goos: "linux"},
		{output: "git version 2.39.1", goos: "linux", want: true},
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
	require := Require.New(t)
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
