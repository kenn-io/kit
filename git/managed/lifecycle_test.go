package managedworktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitenv "go.kenn.io/kit/git/env"
)

func lifecycleGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitenv.StripAll(os.Environ())
	out, err := cmd.CombinedOutput()
	Require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// initLifecycleRepo creates a git repository with one commit on a stable
// default branch named "main" so tests do not depend on the host git's
// init.defaultBranch setting.
func initLifecycleRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := filepath.Join(t.TempDir(), "repo")
	Require.NoError(t, os.MkdirAll(dir, 0o755))
	lifecycleGit(t, dir, "init", "-q", "-b", "main")
	lifecycleGit(t, dir, "config", "user.email", "t@e.st")
	lifecycleGit(t, dir, "config", "user.name", "Tester")
	lifecycleGit(t, dir, "config", "commit.gpgsign", "false")
	lifecycleGit(t, dir, "commit", "--allow-empty", "-m", "initial")
	return dir
}

func branchExistsInRepo(t *testing.T, repo, branch string) bool {
	t.Helper()
	cmd := exec.Command(
		"git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch,
	)
	cmd.Dir = repo
	cmd.Env = gitenv.StripAll(os.Environ())
	return cmd.Run() == nil
}

func canonicalLifecycleTestPath(t *testing.T, path string) string {
	t.Helper()
	clean := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return resolved
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(clean))
	Require.NoError(t, err)
	return filepath.Join(parent, filepath.Base(clean))
}

// writeHookScript writes an executable script that records its working
// directory and lifecycle environment to outFile, exiting with exitCode.
func writeHookScript(t *testing.T, dir, outFile string, exitCode int) string {
	t.Helper()
	script := filepath.Join(dir, "hook")
	shellOutFile := strings.ReplaceAll(filepath.ToSlash(outFile), "'", "'\\''")
	body := "#!/bin/sh\n" +
		"{\n" +
		"  if pwd -W >/dev/null 2>&1; then pwd -W; else pwd; fi\n" +
		"  echo \"name=$KIT_WORKTREE_NAME\"\n" +
		"  echo \"path=$KIT_WORKTREE_PATH\"\n" +
		"  echo \"root=$KIT_PROJECT_ROOT\"\n" +
		"  echo \"branch=$KIT_BRANCH\"\n" +
		"} > '" + shellOutFile + "'\n"
	if exitCode != 0 {
		body += "echo boom >&2\nexit " + string(rune('0'+exitCode)) + "\n"
	}
	Require.NoError(t, os.WriteFile(script, []byte(body), 0o755))
	return script
}

func TestCreateWorktreeOnDiskDerivesPathAndCreatesBranch(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)

	result, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "feat/new-thing",
	})
	require.NoError(err)

	wantBase, evalErr := filepath.EvalSymlinks(repo + "-worktrees")
	require.NoError(evalErr)
	assert.Equal(filepath.Join(wantBase, "feat-new-thing"), result.Path,
		"path derives from <root>-worktrees plus slash-slugged branch")
	assert.Equal("feat/new-thing", result.Branch)
	assert.True(branchExistsInRepo(t, repo, "feat/new-thing"))
	head := lifecycleGit(t, result.Path, "rev-parse", "--abbrev-ref", "HEAD")
	assert.Equal("feat/new-thing", head)
	assert.False(result.HookRan)
}

func TestCreateWorktreeOnDiskCanIsolateUntrustedCheckout(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	require.NoError(os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("payload.txt filter=capture\n"), 0o644))
	require.NoError(os.WriteFile(filepath.Join(repo, "payload.txt"), []byte("content\n"), 0o644))
	lifecycleGit(t, repo, "add", ".gitattributes", "payload.txt")
	lifecycleGit(t, repo, "commit", "-m", "add filtered content")

	marker := filepath.Join(t.TempDir(), "filter-ran")
	script := filepath.Join(t.TempDir(), "filter.sh")
	quotedMarker := "'" + strings.ReplaceAll(filepath.ToSlash(marker), "'", "'\\''") + "'"
	require.NoError(os.WriteFile(script, []byte("#!/bin/sh\nprintf ran > "+quotedMarker+"\nprintf 'filtered:'\ncat\n"), 0o755))
	command := "sh '" + strings.ReplaceAll(filepath.ToSlash(script), "'", "'\\''") + "'"
	lifecycleGit(t, repo, "config", "filter.capture.smudge", command)

	hookMarker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	require.NoError(os.WriteFile(hook, []byte("#!/bin/sh\nprintf ran > '"+filepath.ToSlash(hookMarker)+"'\n"), 0o755))

	path := filepath.Join(t.TempDir(), "isolated")
	validated := false
	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot:      repo,
		Branch:           "review/isolated",
		Path:             path,
		BaseRef:          "HEAD",
		Runner:           gitcmd.New(),
		IsolatedCheckout: true,
		BeforeCheckout: func(_ context.Context, worktreePath string) error {
			validated = true
			assert.NoFileExists(filepath.Join(worktreePath, "payload.txt"))
			return nil
		},
	})
	require.NoError(err)
	assert.True(validated)
	contents, err := os.ReadFile(filepath.Join(result.Path, "payload.txt"))
	require.NoError(err)
	assert.Equal("content\n", string(contents))
	assert.NoFileExists(marker)
	assert.NoFileExists(hookMarker)
}

func TestCreateWorktreeOnDiskIsolatesHooksBeforeBranchCreation(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	hooks := filepath.Join(repo, ".githooks")
	require.NoError(os.MkdirAll(hooks, 0o755))
	marker := filepath.Join(t.TempDir(), "reference-transaction-ran")
	hook := filepath.Join(hooks, "reference-transaction")
	require.NoError(os.WriteFile(hook, []byte(
		"#!/bin/sh\nprintf ran > '"+filepath.ToSlash(marker)+"'\n",
	), 0o755))
	lifecycleGit(t, repo, "config", "core.hooksPath", ".githooks")
	lifecycleGit(t, repo, "branch", "hook-probe")
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		t.Skip("installed Git does not support reference-transaction hooks")
	}
	require.NoError(os.Remove(marker))

	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot:      repo,
		Branch:           "review/hook-isolation",
		Path:             filepath.Join(t.TempDir(), "isolated"),
		BaseRef:          "HEAD",
		IsolatedCheckout: true,
	})
	require.NoError(err)
	t.Cleanup(func() { _, _ = result.Rollback(context.Background()) })
	assert.NoFileExists(marker)
}

func TestCreateWorktreeOnDiskUsesExecutionPolicy(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	hook := filepath.Join(repo, "setup")
	require.NoError(os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	gitCommands := make([]string, 0)
	hookRuns := 0
	result, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot:      repo,
		Branch:           "execution-policy",
		Path:             filepath.Join(t.TempDir(), "wt"),
		SetupScript:      hook,
		IsolatedCheckout: true,
		RunGit: func(
			ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
		) ([]byte, error) {
			gitCommands = append(gitCommands, strings.Join(args, " "))
			stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
			return append(stdout, stderr...), err
		},
		RunHook: func(ctx context.Context, command HookCommand) error {
			hookRuns++
			return nil
		},
	})
	require.NoError(err)
	assert.Equal(1, hookRuns)
	assert.Contains(gitCommands, "config --includes --null --list")
	assert.Contains(gitCommands, "reset --hard HEAD")

	remaining, err := result.Rollback(context.Background())
	require.NoError(err)
	assert.Empty(remaining)
	assert.Contains(gitCommands, "status --porcelain=v1 --untracked-files=all")
	assert.Contains(gitCommands, "worktree remove --force "+result.Path)
	assert.Contains(gitCommands, "update-ref -d refs/heads/"+result.Branch+" "+result.branchOID)
}

func TestCreateWorktreeOnDiskPreservesConfiguredRunnerWithNilEnvironment(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	wantConfig := []gitcmd.Config{{Key: "gc.auto", Value: "0"}}
	sawConfig := false
	sawEnvironment := false

	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "configured-runner",
		Path:        filepath.Join(t.TempDir(), "wt"),
		Runner:      gitcmd.Runner{Config: wantConfig},
		RunGit: func(
			ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
		) ([]byte, error) {
			for _, config := range runner.Config {
				if config == wantConfig[0] {
					sawConfig = true
				}
			}
			sawEnvironment = sawEnvironment || runner.Env != nil
			stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
			return append(stdout, stderr...), err
		},
	})
	require.NoError(err)
	t.Cleanup(func() { _, _ = result.Rollback(context.Background()) })
	assert.True(sawConfig)
	assert.True(sawEnvironment)
}

func TestCreateWorktreeResultRollbackPreservesAdvancedBranch(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "review/advanced",
		Path:        filepath.Join(t.TempDir(), "advanced"),
		BaseRef:     "HEAD",
	})
	require.NoError(err)
	require.NoError(os.WriteFile(filepath.Join(result.Path, "review.txt"), []byte("keep\n"), 0o644))
	lifecycleGit(t, result.Path, "add", "review.txt")
	lifecycleGit(t, result.Path, "commit", "-m", "review work")
	advancedOID := lifecycleGit(t, result.Path, "rev-parse", "HEAD")

	remaining, err := result.Rollback(t.Context())

	require.Error(err)
	assert.Equal(RollbackResult{Path: result.Path, Branch: result.Branch}, remaining)
	assert.FileExists(filepath.Join(result.Path, "review.txt"))
	assert.Equal(advancedOID, lifecycleGit(t, repo, "rev-parse", "refs/heads/"+result.Branch))
}

func TestCreateWorktreeResultRollbackPreservesDirtyWorktree(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "review/dirty",
		Path:        filepath.Join(t.TempDir(), "dirty"),
		BaseRef:     "HEAD",
	})
	require.NoError(err)
	marker := filepath.Join(result.Path, "uncommitted.txt")
	require.NoError(os.WriteFile(marker, []byte("keep\n"), 0o644))

	remaining, err := result.Rollback(t.Context())

	require.Error(err)
	assert.Equal(RollbackResult{Path: result.Path, Branch: result.Branch}, remaining)
	assert.FileExists(marker)
}

func TestCreateWorktreeResultRollbackPreservesReplacedPath(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "review/replaced",
		Path:        filepath.Join(t.TempDir(), "replaced"),
		BaseRef:     "HEAD",
	})
	require.NoError(err)
	require.NoError(os.Rename(result.Path, result.Path+"-original"))
	require.NoError(os.Mkdir(result.Path, 0o755))
	marker := filepath.Join(result.Path, "unrelated.txt")
	require.NoError(os.WriteFile(marker, []byte("keep\n"), 0o644))

	remaining, err := result.Rollback(t.Context())

	require.Error(err)
	assert.Equal(RollbackResult{Path: result.Path, Branch: result.Branch}, remaining)
	assert.FileExists(marker)
}

func TestCreateWorktreeResultRollbackPreservesBranchWhenPathDisappears(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "review/disappeared",
		Path:        filepath.Join(t.TempDir(), "disappeared"),
		BaseRef:     "HEAD",
	})
	require.NoError(err)
	require.NoError(os.RemoveAll(result.Path))

	remaining, err := result.Rollback(t.Context())

	require.Error(err)
	assert.Equal(RollbackResult{Branch: result.Branch}, remaining)
	assert.True(branchExistsInRepo(t, repo, result.Branch))
}

func TestCreateWorktreeResultRollbackDisablesRepositoryHooks(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "review/hooks",
		Path:        filepath.Join(t.TempDir(), "hooks"),
		BaseRef:     "HEAD",
	})
	require.NoError(err)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(repo, ".git", "hooks", "reference-transaction")
	require.NoError(os.WriteFile(hook, []byte("#!/bin/sh\nprintf ran > '"+filepath.ToSlash(marker)+"'\n"), 0o755))

	remaining, err := result.Rollback(t.Context())

	require.NoError(err)
	assert.Empty(remaining)
	assert.NoFileExists(marker)
}

func TestCreateWorktreeOnDiskAttachesExistingBranch(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	lifecycleGit(t, repo, "branch", "existing")
	wantSHA := lifecycleGit(t, repo, "rev-parse", "existing")

	dest := filepath.Join(t.TempDir(), "wt")
	result, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "existing",
		Path:        dest,
	})
	require.NoError(err)
	assert.Equal(dest, result.Path)
	assert.Equal(wantSHA, lifecycleGit(t, dest, "rev-parse", "HEAD"))
	assert.Equal("existing",
		lifecycleGit(t, dest, "rev-parse", "--abbrev-ref", "HEAD"))
}

func TestCreateWorktreeOnDiskFromBaseRef(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	lifecycleGit(t, repo, "checkout", "-q", "-b", "release")
	lifecycleGit(t, repo, "commit", "--allow-empty", "-m", "release work")
	releaseSHA := lifecycleGit(t, repo, "rev-parse", "release")
	lifecycleGit(t, repo, "checkout", "-q", "main")

	dest := filepath.Join(t.TempDir(), "wt")
	result, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "hotfix",
		Path:        dest,
		BaseRef:     "release",
	})
	require.NoError(err)
	assert.Equal(releaseSHA, lifecycleGit(t, dest, "rev-parse", "HEAD"),
		"new branch starts at the base ref, not HEAD")
	assert.Equal("hotfix",
		lifecycleGit(t, dest, "rev-parse", "--abbrev-ref", "HEAD"))
	_ = result
}

func TestCreateWorktreeOnDiskRunsSetupHook(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	outFile := filepath.Join(t.TempDir(), "hook.out")
	script := writeHookScript(t, t.TempDir(), outFile, 0)
	// Hook scripts resolve against the project root; place one inside.
	inRepo := filepath.Join(repo, "setup")
	data, err := os.ReadFile(script)
	require.NoError(err)
	require.NoError(os.WriteFile(inRepo, data, 0o755))

	dest := filepath.Join(t.TempDir(), "wt")
	result, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot:  repo,
		Branch:       "feature",
		Path:         dest,
		SetupScript:  "setup",
		WorktreeName: "Feature Work",
	})
	require.NoError(err)
	assert.True(result.HookRan)
	assert.Equal(inRepo, result.HookScript)

	recorded, err := os.ReadFile(outFile)
	require.NoError(err)
	lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	require.Len(lines, 5)
	// pwd resolves symlinks (macOS /var -> /private/var), so compare
	// canonical forms.
	assert.Equal(canonicalLifecycleTestPath(t, dest), canonicalLifecycleTestPath(t, lines[0]),
		"hook runs in the worktree directory")
	assert.Equal("name=Feature Work", lines[1])
	assert.Equal("path="+dest, lines[2])
	assert.Equal("root="+repo, lines[3])
	assert.Equal("branch=feature", lines[4])
}

func TestCreateWorktreeOnDiskRollsBackWhenSetupHookFails(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	outFile := filepath.Join(t.TempDir(), "hook.out")
	script := writeHookScript(t, t.TempDir(), outFile, 3)
	inRepo := filepath.Join(repo, "setup")
	data, err := os.ReadFile(script)
	require.NoError(err)
	require.NoError(os.WriteFile(inRepo, data, 0o755))

	dest := filepath.Join(t.TempDir(), "wt")
	_, err = CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "feature",
		Path:        dest,
		SetupScript: "setup",
	})
	var hookErr *HookError
	require.ErrorAs(err, &hookErr)
	assert.Equal(3, hookErr.ExitCode)
	assert.Contains(hookErr.Stderr, "boom")

	_, statErr := os.Stat(dest)
	assert.True(os.IsNotExist(statErr),
		"failed hook rolls the worktree directory back")
	assert.False(branchExistsInRepo(t, repo, "feature"),
		"failed hook rolls the created branch back")
}

func TestCreateWorktreeOnDiskRollsBackDirtyOutputFromFailedSetupHook(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	script := filepath.Join(repo, "setup-dirty")
	require.NoError(os.WriteFile(script, []byte(
		"#!/bin/sh\nprintf partial > setup-output.txt\nexit 3\n",
	), 0o755))
	dest := filepath.Join(t.TempDir(), "wt")

	_, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "dirty-setup-failure",
		Path:        dest,
		SetupScript: "setup-dirty",
	})

	var hookErr *HookError
	require.ErrorAs(err, &hookErr)
	_, statErr := os.Stat(dest)
	assert.True(os.IsNotExist(statErr),
		"failed setup output belongs to the operation and is removed")
	assert.False(branchExistsInRepo(t, repo, "dirty-setup-failure"))
}

func TestCreateWorktreeOnDiskKeepsPreexistingBranchOnHookFailure(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	lifecycleGit(t, repo, "branch", "existing")
	script := filepath.Join(repo, "setup")
	require.NoError(os.WriteFile(
		script, []byte("#!/bin/sh\nexit 1\n"), 0o755,
	))

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "existing",
		Path:        dest,
		SetupScript: "setup",
	})
	var hookErr *HookError
	require.ErrorAs(err, &hookErr)
	assert.True(branchExistsInRepo(t, repo, "existing"),
		"a branch the create did not make survives rollback")
}

func TestCreateWorktreeOnDiskRejectsExistingDestination(t *testing.T) {
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := t.TempDir()

	_, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "feature",
		Path:        dest,
	})
	require.ErrorIs(err, ErrWorktreeDestinationExists)
}

func TestCreateWorktreeOnDiskRejectsBranchCheckedOutElsewhere(t *testing.T) {
	require := Require.New(t)
	repo := initLifecycleRepo(t)

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "main",
		Path:        dest,
	})
	require.ErrorIs(err, ErrBranchInUse,
		"main is checked out in the primary worktree")
}

func TestCreateWorktreeOnDiskRejectsHookOutsideProject(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "feature",
		Path:        dest,
		SetupScript: "../outside.sh",
	})
	require.ErrorIs(err, ErrHookOutsideProject)
	_, statErr := os.Stat(dest)
	assert.True(os.IsNotExist(statErr),
		"hook validation happens before any git side effect")
}

func TestCreateWorktreeOnDiskRejectsInvalidBranchName(t *testing.T) {
	require := Require.New(t)
	repo := initLifecycleRepo(t)

	_, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "bad..name",
	})
	require.ErrorIs(err, ErrInvalidBranchName)
}

func TestRemoveWorktreeFromDiskRemovesWorktreeAndBranch(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	lifecycleGit(t, repo, "worktree", "add", "-b", "feature", dest)

	result, err := RemoveWorktreeFromDisk(context.Background(), RemoveWorktreeOptions{
		ProjectRoot:  repo,
		Path:         dest,
		Branch:       "feature",
		RemoveBranch: true,
	})
	require.NoError(err)
	assert.False(result.HookRan)
	_, statErr := os.Stat(dest)
	assert.True(os.IsNotExist(statErr))
	assert.False(branchExistsInRepo(t, repo, "feature"))
}

func TestRemoveWorktreeFromDiskKeepsBranchWithoutRemoveBranch(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	lifecycleGit(t, repo, "worktree", "add", "-b", "feature", dest)

	_, err := RemoveWorktreeFromDisk(context.Background(), RemoveWorktreeOptions{
		ProjectRoot: repo,
		Path:        dest,
		Branch:      "feature",
	})
	require.NoError(err)
	assert.True(branchExistsInRepo(t, repo, "feature"))
}

func TestRemoveWorktreeFromDiskRunsTeardownHookFirst(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	lifecycleGit(t, repo, "worktree", "add", "-b", "feature", dest)

	outFile := filepath.Join(t.TempDir(), "hook.out")
	script := writeHookScript(t, t.TempDir(), outFile, 0)
	inRepo := filepath.Join(repo, "teardown")
	data, err := os.ReadFile(script)
	require.NoError(err)
	require.NoError(os.WriteFile(inRepo, data, 0o755))

	result, err := RemoveWorktreeFromDisk(context.Background(), RemoveWorktreeOptions{
		ProjectRoot:    repo,
		Path:           dest,
		Branch:         "feature",
		TeardownScript: "teardown",
		WorktreeName:   "Feature Work",
	})
	require.NoError(err)
	assert.True(result.HookRan)

	recorded, err := os.ReadFile(outFile)
	require.NoError(err)
	lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	require.Len(lines, 5)
	assert.Equal(canonicalLifecycleTestPath(t, dest), canonicalLifecycleTestPath(t, lines[0]),
		"teardown runs in the worktree before it is removed")
	assert.Equal("name=Feature Work", lines[1])
	assert.Equal("branch=feature", lines[4])
}

func TestRemoveWorktreeFromDiskAbortsWhenTeardownHookFails(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	lifecycleGit(t, repo, "worktree", "add", "-b", "feature", dest)
	script := filepath.Join(repo, "teardown")
	require.NoError(os.WriteFile(
		script, []byte("#!/bin/sh\necho nope >&2\nexit 2\n"), 0o755,
	))

	_, err := RemoveWorktreeFromDisk(context.Background(), RemoveWorktreeOptions{
		ProjectRoot:    repo,
		Path:           dest,
		Branch:         "feature",
		TeardownScript: "teardown",
		RemoveBranch:   true,
	})
	var hookErr *HookError
	require.ErrorAs(err, &hookErr)
	assert.Equal(2, hookErr.ExitCode)
	_, statErr := os.Stat(dest)
	require.NoError(statErr, "failed teardown leaves the worktree in place")
	assert.True(branchExistsInRepo(t, repo, "feature"))
}

func TestRemoveWorktreeFromDiskPrunesWhenPathAlreadyGone(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	lifecycleGit(t, repo, "worktree", "add", "-b", "feature", dest)
	require.NoError(os.RemoveAll(dest))

	_, err := RemoveWorktreeFromDisk(context.Background(), RemoveWorktreeOptions{
		ProjectRoot:  repo,
		Path:         dest,
		Branch:       "feature",
		RemoveBranch: true,
	})
	require.NoError(err)
	assert.False(branchExistsInRepo(t, repo, "feature"))
	list := lifecycleGit(t, repo, "worktree", "list", "--porcelain")
	assert.NotContains(list, dest, "stale worktree entry pruned")
}

func TestRemoveMissingWorktreePreservesUnrelatedStaleRegistration(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	target := filepath.Join(t.TempDir(), "target")
	unrelated := filepath.Join(t.TempDir(), "unrelated")
	lifecycleGit(t, repo, "worktree", "add", "-b", "target", target)
	lifecycleGit(t, repo, "worktree", "add", "-b", "unrelated", unrelated)
	require.NoError(os.RemoveAll(target))
	require.NoError(os.RemoveAll(unrelated))

	_, err := RemoveWorktreeFromDisk(context.Background(), RemoveWorktreeOptions{
		ProjectRoot:  repo,
		Path:         target,
		Branch:       "target",
		RemoveBranch: true,
	})

	require.NoError(err)
	list := lifecycleGit(t, repo, "worktree", "list", "--porcelain")
	assert.NotContains(list, "branch refs/heads/target")
	assert.Contains(list, "branch refs/heads/unrelated",
		"removing one stale worktree must not prune another registration")
}

func TestRemoveWorktreeFromDiskForceRemovesDirtyWorktree(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	lifecycleGit(t, repo, "worktree", "add", "-b", "feature", dest)
	require.NoError(os.WriteFile(
		filepath.Join(dest, "dirty.txt"), []byte("x\n"), 0o644,
	))

	_, err := RemoveWorktreeFromDisk(context.Background(), RemoveWorktreeOptions{
		ProjectRoot: repo,
		Path:        dest,
		Branch:      "feature",
		Force:       true,
	})
	require.NoError(err)
	_, statErr := os.Stat(dest)
	assert.True(os.IsNotExist(statErr))
}

func TestRemoveWorktreeFromDiskForceRemovesLockedWorktree(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	lifecycleGit(t, repo, "worktree", "add", "-b", "locked", dest)
	lifecycleGit(t, repo, "worktree", "lock", dest)

	_, err := RemoveWorktreeFromDisk(t.Context(), RemoveWorktreeOptions{
		ProjectRoot: repo,
		Path:        dest,
		Branch:      "locked",
		Force:       true,
	})

	require.NoError(err)
	assert.NoDirExists(dest)
}

func TestRemoveWorktreeFromDiskForceRemovesMissingLockedWorktree(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	lifecycleGit(t, repo, "worktree", "add", "-b", "locked-missing", dest)
	lifecycleGit(t, repo, "worktree", "lock", dest)
	require.NoError(os.RemoveAll(dest))

	_, err := RemoveWorktreeFromDisk(t.Context(), RemoveWorktreeOptions{
		ProjectRoot: repo,
		Path:        dest,
		Branch:      "locked-missing",
		Force:       true,
	})

	require.NoError(err)
	list := lifecycleGit(t, repo, "worktree", "list", "--porcelain")
	assert.NotContains(list, "branch refs/heads/locked-missing")
}

func TestRemoveWorktreeFromDiskBranchInUseElsewhere(t *testing.T) {
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	lifecycleGit(t, repo, "worktree", "add", "-b", "feature", dest)

	_, err := RemoveWorktreeFromDisk(context.Background(), RemoveWorktreeOptions{
		ProjectRoot:  repo,
		Path:         dest,
		Branch:       "main",
		RemoveBranch: true,
	})
	require.ErrorIs(err, ErrBranchInUse,
		"deleting a branch checked out in another worktree is refused")
}

func TestWorktreeIsDirty(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	lifecycleGit(t, repo, "worktree", "add", "-b", "feature", dest)

	dirty, err := WorktreeIsDirty(context.Background(), dest)
	require.NoError(err)
	assert.False(dirty)

	require.NoError(os.WriteFile(
		filepath.Join(dest, "scratch.txt"), []byte("x\n"), 0o644,
	))
	dirty, err = WorktreeIsDirty(context.Background(), dest)
	require.NoError(err)
	assert.True(dirty)

	_, err = WorktreeIsDirty(context.Background(), filepath.Join(dest, "missing"))
	require.Error(err, "a missing path is an error, not clean")
}
