package managedworktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// writeHookScript writes an executable script that records its working
// directory and lifecycle environment to outFile, exiting with exitCode.
func writeHookScript(t *testing.T, dir, outFile string, exitCode int) string {
	t.Helper()
	script := filepath.Join(dir, "hook.sh")
	body := "#!/bin/sh\n" +
		"{\n" +
		"  pwd\n" +
		"  echo \"name=$KIT_WORKTREE_NAME\"\n" +
		"  echo \"path=$KIT_WORKTREE_PATH\"\n" +
		"  echo \"root=$KIT_PROJECT_ROOT\"\n" +
		"  echo \"branch=$KIT_BRANCH\"\n" +
		"} > \"" + filepath.ToSlash(outFile) + "\"\n"
	if exitCode != 0 {
		body += "echo boom >&2\nexit " + string(rune('0'+exitCode)) + "\n"
	}
	Require.NoError(t, os.WriteFile(script, []byte(body), 0o755))
	return script
}

func testHookRunner() HookRunner {
	if runtime.GOOS != "windows" {
		return nil
	}
	return runTestHook
}

func runTestHook(ctx context.Context, command HookCommand) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "sh", command.Script)
	} else {
		cmd = exec.CommandContext(ctx, command.Script)
	}
	cmd.Dir = command.Dir
	cmd.Env = command.Env
	cmd.Stdout = command.Stdout
	cmd.Stderr = command.Stderr
	return cmd.Run()
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

func TestCreateWorktreeOnDiskReportsBranchCreated(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	lifecycleGit(t, repo, "branch", "existing")

	attached, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "existing",
		Path:        filepath.Join(t.TempDir(), "wt-existing"),
	})
	require.NoError(err)
	assert.False(attached.BranchCreated)

	created, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "brand-new",
		Path:        filepath.Join(t.TempDir(), "wt-new"),
	})
	require.NoError(err)
	assert.True(created.BranchCreated)
}

func TestCreateWorktreeResultRollbackPreservesPreexistingBranch(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	lifecycleGit(t, repo, "branch", "keep-me")

	dest := filepath.Join(t.TempDir(), "wt")
	result, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "keep-me",
		Path:        dest,
	})
	require.NoError(err)

	remaining, err := result.Rollback(context.Background())
	require.NoError(err)
	assert.Empty(remaining)
	_, statErr := os.Stat(dest)
	assert.True(os.IsNotExist(statErr))
	assert.True(branchExistsInRepo(t, repo, "keep-me"))
}

func TestCreateWorktreeOnDiskRejectsSymlinkedHookEscape(t *testing.T) {
	require := Require.New(t)
	repo := initLifecycleRepo(t)

	outside := filepath.Join(t.TempDir(), "outside.sh")
	require.NoError(os.WriteFile(
		outside, []byte("#!/bin/sh\nexit 0\n"), 0o755,
	))
	link := filepath.Join(repo, "hook-link.sh")
	require.NoError(os.Symlink(outside, link))

	_, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "feat/escape",
		SetupScript: "hook-link.sh",
	})
	require.ErrorIs(err, ErrHookOutsideProject)
}

func TestCreateWorktreeOnDiskUsesExecutionPolicy(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	hook := filepath.Join(repo, "setup.sh")
	require.NoError(os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	gitRuns := 0
	hookRuns := 0
	_, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "execution-policy",
		Path:        filepath.Join(t.TempDir(), "wt"),
		SetupScript: hook,
		RunGit: func(
			ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
		) ([]byte, error) {
			gitRuns++
			stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
			return append(stdout, stderr...), err
		},
		RunHook: func(ctx context.Context, command HookCommand) error {
			hookRuns++
			return runTestHook(ctx, command)
		},
	})
	require.NoError(err)
	assert.Greater(gitRuns, 0)
	assert.Equal(1, hookRuns)
}

func TestCreateWorktreeOnDiskPreservesRunnerConfigurationWithNilEnv(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)

	configSeen := false
	_, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "configured-runner",
		Path:        filepath.Join(t.TempDir(), "wt"),
		Runner: gitcmd.Runner{
			Config: []gitcmd.Config{{Key: "advice.detachedHead", Value: "false"}},
		},
		RunGit: func(
			ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
		) ([]byte, error) {
			configSeen = configSeen || assert.Contains(
				runner.Config,
				gitcmd.Config{Key: "advice.detachedHead", Value: "false"},
			)
			stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
			return append(stdout, stderr...), err
		},
	})
	require.NoError(err)
	assert.True(configSeen)
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

func TestCreateWorktreeResultRollbackPreservesDetachedCommit(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "review/detached",
		Path:        filepath.Join(t.TempDir(), "detached"),
		BaseRef:     "HEAD",
	})
	require.NoError(err)
	lifecycleGit(t, result.Path, "checkout", "--detach")
	lifecycleGit(t, result.Path, "commit", "--allow-empty", "-m", "detached work")
	detachedOID := lifecycleGit(t, result.Path, "rev-parse", "HEAD")

	remaining, err := result.Rollback(t.Context())

	require.ErrorIs(err, ErrWorktreeCleanupIncomplete)
	assert.Equal(RollbackResult{Path: result.Path, Branch: result.Branch}, remaining)
	assert.Equal(detachedOID, lifecycleGit(t, result.Path, "rev-parse", "HEAD"))
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

func TestCreateWorktreeResultRollbackPreservesIgnoredArtifacts(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	require.NoError(os.WriteFile(
		filepath.Join(repo, ".gitignore"), []byte("scratch.log\n"), 0o644,
	))
	lifecycleGit(t, repo, "add", ".gitignore")
	lifecycleGit(t, repo, "commit", "-qm", "ignore scratch artifacts")

	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "ignored-artifact",
	})
	require.NoError(err)
	require.NoError(os.WriteFile(
		filepath.Join(result.Path, "scratch.log"), []byte("keep me\n"), 0o600,
	))

	remaining, err := result.Rollback(t.Context())

	require.ErrorIs(err, ErrWorktreeCleanupIncomplete)
	assert.Equal(RollbackResult{Path: result.Path, Branch: result.Branch}, remaining)
	assert.FileExists(filepath.Join(result.Path, "scratch.log"))
}

func TestCreateWorktreeOnDiskReportsSnapshotCleanupFailure(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	runGit := func(
		ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
	) ([]byte, error) {
		if len(args) >= 2 && args[0] == "rev-parse" &&
			args[1] == "--verify" && dir != repo {
			return nil, errors.New("snapshot failed")
		}
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
			return nil, errors.New("cleanup failed")
		}
		stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
		return append(stdout, stderr...), err
	}

	_, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "snapshot-cleanup-failure",
		RunGit:      runGit,
	})

	require.Error(err)
	assert.ErrorContains(err, "snapshot failed")
	assert.ErrorContains(err, "cleanup failed")
}

func TestLifecycleWorktreeHeadPropagatesSymbolicRefFailure(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	ctx := withLifecycleExecution(
		t.Context(), gitcmd.Runner{}, func(
			ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
		) ([]byte, error) {
			if len(args) > 0 && args[0] == "symbolic-ref" {
				return nil, errors.New("symbolic ref failed")
			}
			stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
			return append(stdout, stderr...), err
		}, nil,
	)

	_, _, err := lifecycleWorktreeHead(ctx, repo)

	require.Error(err)
	assert.ErrorContains(err, "symbolic ref failed")
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
	inRepo := filepath.Join(repo, "setup.sh")
	data, err := os.ReadFile(script)
	require.NoError(err)
	require.NoError(os.WriteFile(inRepo, data, 0o755))

	dest := filepath.Join(t.TempDir(), "wt")
	result, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot:  repo,
		Branch:       "feature",
		Path:         dest,
		SetupScript:  "setup.sh",
		WorktreeName: "Feature Work",
		RunHook:      testHookRunner(),
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
	canonicalDest, err := filepath.EvalSymlinks(dest)
	require.NoError(err)
	assert.Equal(canonicalDest, lines[0],
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
	inRepo := filepath.Join(repo, "setup.sh")
	data, err := os.ReadFile(script)
	require.NoError(err)
	require.NoError(os.WriteFile(inRepo, data, 0o755))

	dest := filepath.Join(t.TempDir(), "wt")
	_, err = CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "feature",
		Path:        dest,
		SetupScript: "setup.sh",
		RunHook:     testHookRunner(),
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

func TestCreateWorktreeOnDiskKeepsPreexistingBranchOnHookFailure(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	lifecycleGit(t, repo, "branch", "existing")
	script := filepath.Join(repo, "setup.sh")
	require.NoError(os.WriteFile(
		script, []byte("#!/bin/sh\nexit 1\n"), 0o755,
	))

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "existing",
		Path:        dest,
		SetupScript: "setup.sh",
		RunHook:     testHookRunner(),
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
	inRepo := filepath.Join(repo, "teardown.sh")
	data, err := os.ReadFile(script)
	require.NoError(err)
	require.NoError(os.WriteFile(inRepo, data, 0o755))

	result, err := RemoveWorktreeFromDisk(context.Background(), RemoveWorktreeOptions{
		ProjectRoot:    repo,
		Path:           dest,
		Branch:         "feature",
		TeardownScript: "teardown.sh",
		WorktreeName:   "Feature Work",
		RunHook:        testHookRunner(),
	})
	require.NoError(err)
	assert.True(result.HookRan)

	recorded, err := os.ReadFile(outFile)
	require.NoError(err)
	lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	require.Len(lines, 5)
	canonicalDest, err := filepath.EvalSymlinks(dest)
	// The worktree is gone by the time we compare; EvalSymlinks on a
	// removed path fails, so canonicalize the parent instead.
	if err != nil {
		parent, evalErr := filepath.EvalSymlinks(filepath.Dir(dest))
		require.NoError(evalErr)
		canonicalDest = filepath.Join(parent, filepath.Base(dest))
	}
	assert.Equal(canonicalDest, lines[0],
		"teardown runs in the worktree before it is removed")
	assert.Equal("name=Feature Work", lines[1])
	assert.Equal("branch=feature", lines[4])
}

func TestRemoveWorktreeFromDiskRejectsMismatchedBranchBeforeHook(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "actual-branch",
	})
	require.NoError(err)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	writeHookScript(t, repo, marker, 0)

	_, err = RemoveWorktreeFromDisk(t.Context(), RemoveWorktreeOptions{
		ProjectRoot:    repo,
		Path:           created.Path,
		Branch:         "stale-record-branch",
		Force:          true,
		RemoveBranch:   true,
		TeardownScript: "hook.sh",
	})

	require.Error(err)
	assert.NoFileExists(marker)
	assert.DirExists(created.Path)
	assert.True(branchExistsInRepo(t, repo, "actual-branch"))
}

func TestRemoveWorktreeFromDiskRejectsDifferentRepositoryBeforeHook(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	otherRepo := initLifecycleRepo(t)
	created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: otherRepo,
		Branch:      "other-repository",
	})
	require.NoError(err)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	writeHookScript(t, repo, marker, 0)

	_, err = RemoveWorktreeFromDisk(t.Context(), RemoveWorktreeOptions{
		ProjectRoot:    repo,
		Path:           created.Path,
		Branch:         created.Branch,
		Force:          true,
		RemoveBranch:   true,
		TeardownScript: "hook.sh",
	})

	require.Error(err)
	assert.NoFileExists(marker)
	assert.DirExists(created.Path)
	assert.True(branchExistsInRepo(t, otherRepo, created.Branch))
}

func TestRemoveWorktreeFromDiskAbortsWhenTeardownHookFails(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	lifecycleGit(t, repo, "worktree", "add", "-b", "feature", dest)
	script := filepath.Join(repo, "teardown.sh")
	require.NoError(os.WriteFile(
		script, []byte("#!/bin/sh\necho nope >&2\nexit 2\n"), 0o755,
	))

	_, err := RemoveWorktreeFromDisk(context.Background(), RemoveWorktreeOptions{
		ProjectRoot:    repo,
		Path:           dest,
		Branch:         "feature",
		TeardownScript: "teardown.sh",
		RemoveBranch:   true,
		RunHook:        testHookRunner(),
	})
	var hookErr *HookError
	require.ErrorAs(err, &hookErr)
	assert.Equal(2, hookErr.ExitCode)
	_, statErr := os.Stat(dest)
	require.NoError(statErr, "failed teardown leaves the worktree in place")
	assert.True(branchExistsInRepo(t, repo, "feature"))
}

func TestLifecycleHookPreservesContextCancellation(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	script := filepath.Join(repo, "hook")
	require.NoError(os.WriteFile(script, []byte("fixture"), 0o755))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	err := runLifecycleHook(
		withLifecycleExecution(ctx, gitcmd.Runner{}, nil, func(
			_ context.Context, _ HookCommand,
		) error {
			cancel()
			cmd := exec.Command(
				os.Args[0], "-test.run=^TestLifecycleHookExitHelper$",
			)
			cmd.Env = append(os.Environ(), "KIT_TEST_HOOK_EXIT=1")
			return cmd.Run()
		}),
		script, repo, repo, "branch", "", "",
	)

	require.Error(err)
	assert.ErrorIs(err, context.Canceled)
	var hookErr *HookError
	assert.False(errors.As(err, &hookErr))
}

func TestLifecycleHookExitHelper(t *testing.T) {
	if os.Getenv("KIT_TEST_HOOK_EXIT") != "1" {
		t.Skip("helper process")
	}
	os.Exit(7)
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

func TestRemoveWorktreeFromDiskRejectsMismatchedStaleRegistration(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	lifecycleGit(t, repo, "worktree", "add", "-b", "registered-branch", dest)
	require.NoError(os.RemoveAll(dest))

	_, err := RemoveWorktreeFromDisk(t.Context(), RemoveWorktreeOptions{
		ProjectRoot:  repo,
		Path:         dest,
		Branch:       "unrelated-branch",
		RemoveBranch: true,
	})

	require.Error(err)
	assert.True(branchExistsInRepo(t, repo, "registered-branch"))
	assert.Contains(lifecycleGit(t, repo, "worktree", "list", "--porcelain"),
		"branch refs/heads/registered-branch")
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
	assert.Contains(list, "branch refs/heads/unrelated")
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

func TestWorktreeIsDirtyIncludesUntrackedFilesWhenConfigHidesThem(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	lifecycleGit(t, repo, "config", "status.showUntrackedFiles", "no")
	require.NoError(os.WriteFile(
		filepath.Join(repo, "untracked.txt"), []byte("keep\n"), 0o644,
	))

	dirty, err := WorktreeIsDirty(t.Context(), repo)

	require.NoError(err)
	assert.True(dirty)
}
