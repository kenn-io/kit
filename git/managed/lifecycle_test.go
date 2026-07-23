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
	"time"

	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitenv "go.kenn.io/kit/git/env"
	"go.kenn.io/kit/safefileio"
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

func configureLifecycleGitIdentity(t *testing.T, dir string) {
	t.Helper()
	lifecycleGit(t, dir, "config", "user.email", "t@e.st")
	lifecycleGit(t, dir, "config", "user.name", "Tester")
	lifecycleGit(t, dir, "config", "commit.gpgsign", "false")
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
	configureLifecycleGitIdentity(t, dir)
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

func TestLifecycleShebangCommandUsesDeclaredInterpreterRegardlessOfExtension(t *testing.T) {
	for _, test := range []struct {
		name            string
		content         string
		wantInterpreter string
		wantArgs        []string
		wantOK          bool
	}{
		{name: "extension shell", content: "#!/bin/sh\necho ok\n", wantInterpreter: "sh", wantOK: true},
		{name: "env python", content: "#!/usr/bin/env python3 -u\nprint(1)\n", wantInterpreter: "python3", wantArgs: []string{"-u"}, wantOK: true},
		{name: "env split", content: "#!/usr/bin/env -S python3 -u\nprint(1)\n", wantInterpreter: "python3", wantArgs: []string{"-u"}, wantOK: true},
		{name: "no shebang", content: "echo ok\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := Require.New(t)
			extension := ".sh"
			if test.wantInterpreter == "python3" {
				extension = ".py"
			}
			script := filepath.Join(t.TempDir(), "setup"+extension)
			require.NoError(os.WriteFile(script, []byte(test.content), 0o755))

			interpreter, args, ok := lifecycleShebangCommand(script)

			assert.Equal(test.wantOK, ok)
			assert.Equal(test.wantInterpreter, interpreter)
			if test.wantOK {
				assert.Equal(append(test.wantArgs, script), args)
			} else {
				assert.Empty(args)
			}
		})
	}
}

func TestShebangExecutableNamePreservesWindowsAbsoluteInterpreter(t *testing.T) {
	assert.Equal(t, `C:\Tools\python.exe`,
		shebangExecutableNameForOS(`C:\Tools\python.exe`, "windows"))
	assert.Equal(t, `C:/Tools/python.exe`,
		shebangExecutableNameForOS(`C:/Tools/python.exe`, "windows"))
	assert.Equal(t, "python.exe",
		shebangExecutableNameForOS(`/usr/bin/python.exe`, "windows"))
}

func TestLifecycleHookSnapshotModeKeepsWindowsSnapshotDeletable(t *testing.T) {
	assert.NotZero(t, lifecycleHookSnapshotMode("windows")&0o200)
	assert.Equal(t, os.FileMode(0o500), lifecycleHookSnapshotMode("linux"))
}

func TestLifecycleHookSnapshotCleanupRemovesFile(t *testing.T) {
	require := Require.New(t)
	script := lifecycleHookScript{path: filepath.Join(t.TempDir(), "setup.sh")}

	snapshot, cleanup, err := script.executableSnapshot([]byte("#!/bin/sh\nexit 0\n"))
	require.NoError(err)
	require.FileExists(snapshot)

	cleanup()
	assert.NoFileExists(t, snapshot)
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

func TestCreateWorktreeOnDiskSecuresDefaultBase(t *testing.T) {
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	base := repo + "-worktrees"
	require.NoError(os.MkdirAll(base, 0o755))
	require.NoError(os.Chmod(base, 0o755))

	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "private-default-base",
	})

	require.NoError(err)
	t.Cleanup(func() { _, _ = result.Rollback(context.Background()) })
	require.NoError(safefileio.ValidatePrivateDir(base))
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

func TestCreateWorktreeOnDiskRejectsUnsafeGitBeforeIsolatedMutation(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	path := filepath.Join(t.TempDir(), "unsafe-git")
	mutated := false

	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot:      repo,
		Branch:           "review/unsafe-git",
		Path:             path,
		BaseRef:          "HEAD",
		IsolatedCheckout: true,
		RunGit: func(
			ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
		) ([]byte, error) {
			if len(args) == 1 && args[0] == "version" {
				return []byte("git version 2.38.5\n"), nil
			}
			if len(args) >= 2 && args[0] == "worktree" && args[1] == "add" {
				mutated = true
			}
			stdout, stderr, runErr := runner.Run(ctx, dir, nil, args...)
			return append(stdout, stderr...), runErr
		},
	})
	if result.Path != "" {
		t.Cleanup(func() { _, _ = result.Rollback(context.Background()) })
	}

	require.Error(err)
	assert.Contains(err.Error(), "isolated checkout requires "+safeCheckoutGitVersionRequirement(runtime.GOOS))
	assert.False(mutated)
	assert.NoDirExists(path)
	assert.False(branchExistsInRepo(t, repo, "review/unsafe-git"))
}

func TestCreateWorktreeOnDiskPreservesArtifactFromFailedBeforeCheckout(t *testing.T) {
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "pre-checkout-artifact")
	wantErr := errors.New("validation failed")

	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot:      repo,
		Branch:           "pre-checkout-artifact",
		Path:             dest,
		BaseRef:          "HEAD",
		IsolatedCheckout: true,
		BeforeCheckout: func(_ context.Context, worktreePath string) error {
			Require.NoError(t, os.WriteFile(
				filepath.Join(worktreePath, "keep.txt"), []byte("keep\n"), 0o600,
			))
			return wantErr
		},
	})

	Require.ErrorIs(t, err, wantErr)
	Require.ErrorIs(t, err, ErrWorktreeCleanupIncomplete)
	assert.Equal(t, dest, result.Path)
	assert.FileExists(t, filepath.Join(dest, "keep.txt"))
	assert.True(t, branchExistsInRepo(t, repo, "pre-checkout-artifact"))
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
	assert.Contains(gitCommands, "status --porcelain=v1 --untracked-files=all --ignored=matching")
	assert.Contains(gitCommands, "worktree remove "+result.Path)
	assert.Contains(gitCommands, "update-ref --no-deref -d refs/heads/"+result.Branch+" "+result.branchOID)
}

func TestCreateWorktreeOnDiskSerializesRepositoryMutation(t *testing.T) {
	repo := initLifecycleRepo(t)
	entered := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	runGit := func(
		ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
	) ([]byte, error) {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "add" {
			entered <- struct{}{}
			<-release
		}
		stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
		return append(stdout, stderr...), err
	}
	type createResult struct {
		result CreateWorktreeResult
		err    error
	}
	results := make(chan createResult, 2)
	started := make(chan struct{}, 2)
	create := func(branch, path string) {
		started <- struct{}{}
		result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
			ProjectRoot: repo,
			Branch:      branch,
			Path:        path,
			RunGit:      runGit,
		})
		results <- createResult{result: result, err: err}
	}
	firstPath := filepath.Join(t.TempDir(), "serialized-one")
	secondPath := filepath.Join(t.TempDir(), "serialized-two")
	go create("serialized-one", firstPath)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first worktree mutation did not start")
	}
	go create("serialized-two", secondPath)
	<-started
	<-started
	select {
	case <-entered:
		release <- struct{}{}
		release <- struct{}{}
		first := <-results
		second := <-results
		if first.err == nil {
			_, _ = first.result.Rollback(context.Background())
		}
		if second.err == nil {
			_, _ = second.result.Rollback(context.Background())
		}
		t.Fatal("second worktree mutation entered before the first released its repository lock")
	case <-time.After(150 * time.Millisecond):
	}
	release <- struct{}{}
	first := <-results
	Require.NoError(t, first.err)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("second worktree mutation did not resume")
	}
	release <- struct{}{}
	second := <-results
	Require.NoError(t, second.err)
	for _, result := range []CreateWorktreeResult{first.result, second.result} {
		_, err := result.Rollback(context.Background())
		Require.NoError(t, err)
	}
}

func TestRepositoryMutationLockUsesCommonGitDirectory(t *testing.T) {
	repo := initLifecycleRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	lifecycleGit(t, repo, "worktree", "add", "-b", "linked-lock-test", linked)
	t.Cleanup(func() {
		_, _ = RemoveWorktreeFromDisk(context.Background(), RemoveWorktreeOptions{ProjectRoot: repo, Path: linked, Branch: "linked-lock-test", Force: true, RemoveBranch: true})
	})

	_, unlock, err := acquireRepositoryMutationLock(t.Context(), repo)
	Require.NoError(t, err)
	acquired := make(chan func() error, 1)
	errs := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, release, lockErr := acquireRepositoryMutationLock(t.Context(), linked)
		if lockErr != nil {
			errs <- lockErr
			return
		}
		acquired <- release
	}()
	<-started
	select {
	case release := <-acquired:
		_ = release()
		_ = unlock()
		t.Fatal("linked worktree acquired a different repository lock")
	case lockErr := <-errs:
		_ = unlock()
		Require.NoError(t, lockErr)
	case <-time.After(150 * time.Millisecond):
	}
	Require.NoError(t, unlock())
	select {
	case release := <-acquired:
		Require.NoError(t, release())
	case lockErr := <-errs:
		Require.NoError(t, lockErr)
	case <-time.After(5 * time.Second):
		t.Fatal("linked worktree did not acquire the repository lock after release")
	}
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

func TestCreateWorktreeOnDiskPreservesArtifactsWhenSnapshotFails(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "snapshot-failure")
	snapshotFailure := errors.New("snapshot unavailable")

	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "snapshot-failure",
		Path:        dest,
		RunGit: func(
			ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
		) ([]byte, error) {
			if len(args) == 3 && args[0] == "rev-parse" && args[1] == "--verify" &&
				args[2] == "refs/heads/snapshot-failure^{commit}" {
				return nil, snapshotFailure
			}
			stdout, stderr, runErr := runner.Run(ctx, dir, nil, args...)
			return append(stdout, stderr...), runErr
		},
	})

	require.ErrorIs(err, snapshotFailure)
	require.ErrorIs(err, ErrWorktreeCleanupIncomplete)
	assert.Equal(dest, result.Path)
	assert.Equal("snapshot-failure", result.Branch)
	assert.DirExists(dest)
	assert.True(branchExistsInRepo(t, repo, "snapshot-failure"))

	remaining, rollbackErr := result.Rollback(t.Context())
	require.ErrorIs(rollbackErr, ErrWorktreeCleanupIncomplete)
	assert.Equal(RollbackResult{Path: dest, Branch: "snapshot-failure"}, remaining)
	assert.DirExists(dest)
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

	require.ErrorIs(err, ErrWorktreeCleanupIncomplete)
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

func TestCreateWorktreeResultRollbackPreservesFileCreatedAfterFinalStatusCheck(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	var result CreateWorktreeResult
	armed := false
	changed := false
	statusChecks := 0
	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "review/status-race",
		Path:        filepath.Join(t.TempDir(), "status-race"),
		BaseRef:     "HEAD",
		RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
			stdout, stderr, runErr := runner.Run(ctx, dir, nil, args...)
			if armed && runErr == nil && len(args) > 0 && args[0] == "status" {
				statusChecks++
				if statusChecks == 2 {
					changed = true
					require.NoError(os.WriteFile(
						filepath.Join(result.Path, "keep.txt"), []byte("keep\n"), 0o600,
					))
				}
			}
			return append(stdout, stderr...), runErr
		},
	})
	require.NoError(err)
	armed = true

	remaining, err := result.Rollback(t.Context())

	require.ErrorIs(err, ErrWorktreeCleanupIncomplete)
	assert.True(changed)
	assert.Equal(2, statusChecks)
	assert.Equal(RollbackResult{Path: result.Path, Branch: result.Branch}, remaining)
	assert.FileExists(filepath.Join(result.Path, "keep.txt"))
}

func TestCreateWorktreeResultRollbackPreservesHeadChangedBeforeRemoval(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	var result CreateWorktreeResult
	armed := false
	changed := false
	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "review/head-race",
		Path:        filepath.Join(t.TempDir(), "head-race"),
		BaseRef:     "HEAD",
		RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
			stdout, stderr, runErr := runner.Run(ctx, dir, nil, args...)
			if armed && !changed && runErr == nil && len(args) >= 3 &&
				args[0] == "rev-parse" && args[1] == "--abbrev-ref" && args[2] == "HEAD" {
				changed = true
				require.NoError(os.WriteFile(
					filepath.Join(result.Path, "keep.txt"), []byte("keep\n"), 0o600,
				))
				lifecycleGit(t, result.Path, "add", "keep.txt")
				lifecycleGit(t, result.Path, "commit", "-m", "keep concurrent work")
			}
			return append(stdout, stderr...), runErr
		},
	})
	require.NoError(err)
	armed = true

	remaining, err := result.Rollback(t.Context())

	require.ErrorIs(err, ErrWorktreeCleanupIncomplete)
	assert.True(changed)
	assert.Equal(RollbackResult{Path: result.Path, Branch: result.Branch}, remaining)
	assert.FileExists(filepath.Join(result.Path, "keep.txt"))
}

func TestCreateWorktreeResultRollbackPreservesIgnoredFiles(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	require.NoError(os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.txt\n"), 0o644))
	lifecycleGit(t, repo, "add", ".gitignore")
	lifecycleGit(t, repo, "commit", "-m", "ignore local artifact")
	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "review/ignored-file",
		Path:        filepath.Join(t.TempDir(), "ignored"),
		BaseRef:     "HEAD",
	})
	require.NoError(err)
	ignored := filepath.Join(result.Path, "ignored.txt")
	require.NoError(os.WriteFile(ignored, []byte("keep\n"), 0o644))

	remaining, err := result.Rollback(t.Context())

	require.ErrorIs(err, ErrWorktreeCleanupIncomplete)
	assert.Equal(RollbackResult{Path: result.Path, Branch: result.Branch}, remaining)
	assert.FileExists(ignored)
}

func TestLifecycleHooksDirIsPrivate(t *testing.T) {
	require := Require.New(t)
	hooksDir, err := lifecycleHooksDir()
	require.NoError(err)
	require.NoError(safefileio.ValidatePrivateDir(hooksDir))
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

func TestCreateWorktreeResultRollbackPreservesReplacementRepository(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	removeAttempts := 0
	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "review/replacement-repository",
		Path:        filepath.Join(t.TempDir(), "replacement-repository"),
		BaseRef:     "HEAD",
		RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
				removeAttempts++
				return nil, nil
			}
			stdout, stderr, runErr := runner.Run(ctx, dir, nil, args...)
			return append(stdout, stderr...), runErr
		},
	})
	require.NoError(err)
	replacement := filepath.Join(t.TempDir(), "other-repository")
	lifecycleGit(t, filepath.Dir(replacement), "clone", "-q", repo, replacement)
	lifecycleGit(t, replacement, "checkout", "-q", "-b", result.Branch,
		"origin/"+result.Branch)
	require.NoError(os.WriteFile(filepath.Join(result.Path, ".git"), []byte(
		"gitdir: "+filepath.ToSlash(filepath.Join(replacement, ".git"))+"\n",
	), 0o600))

	remaining, err := result.Rollback(t.Context())

	require.ErrorIs(err, ErrWorktreeCleanupIncomplete)
	assert.Equal(RollbackResult{Path: result.Path, Branch: result.Branch}, remaining)
	assert.Zero(removeAttempts, "rollback must verify repository ownership before removal")
	assert.DirExists(result.Path)
}

func TestCreateWorktreeResultRollbackPreservesSymbolicBranchReplacement(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	branch := "review/symbolic-replacement"
	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      branch,
		Path:        filepath.Join(t.TempDir(), "symbolic-replacement"),
		BaseRef:     "HEAD",
		RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
			stdout, stderr, runErr := runner.Run(ctx, dir, nil, args...)
			if runErr == nil && len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
				lifecycleGit(t, repo, "update-ref", "-d", "refs/heads/"+branch)
				lifecycleGit(t, repo, "symbolic-ref", "refs/heads/"+branch, "refs/heads/main")
			}
			return append(stdout, stderr...), runErr
		},
	})
	require.NoError(err)
	mainOID := lifecycleGit(t, repo, "rev-parse", "refs/heads/main")

	remaining, err := result.Rollback(t.Context())

	require.ErrorIs(err, ErrWorktreeCleanupIncomplete)
	assert.Equal(branch, remaining.Branch)
	require.True(branchExistsInRepo(t, repo, "main"),
		"rollback must not dereference and delete the symbolic target")
	assert.Equal(mainOID, lifecycleGit(t, repo, "rev-parse", "refs/heads/main"),
		"rollback must not dereference and delete the symbolic target")
	assert.Equal("refs/heads/main",
		lifecycleGit(t, repo, "symbolic-ref", "refs/heads/"+branch))
}

func TestCreateWorktreeResultRollbackPreservesDetachedCommit(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "review/detached-commit",
		Path:        filepath.Join(t.TempDir(), "detached"),
		BaseRef:     "HEAD",
	})
	require.NoError(err)
	lifecycleGit(t, result.Path, "checkout", "--detach")
	lifecycleGit(t, result.Path, "commit", "--allow-empty", "-m", "detached work")
	detachedOID := lifecycleGit(t, result.Path, "rev-parse", "HEAD")

	remaining, err := result.Rollback(t.Context())

	require.Error(err)
	assert.Equal(RollbackResult{Path: result.Path, Branch: result.Branch}, remaining)
	assert.DirExists(result.Path)
	assert.Equal(detachedOID, lifecycleGit(t, result.Path, "rev-parse", "HEAD"))
}

func TestCreateWorktreeResultRollbackPreservesDetachedHeadAtOriginalCommit(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "review/detached-head",
		Path:        filepath.Join(t.TempDir(), "detached"),
		BaseRef:     "HEAD",
	})
	require.NoError(err)
	lifecycleGit(t, result.Path, "checkout", "--detach")

	remaining, err := result.Rollback(t.Context())

	require.Error(err)
	assert.Equal(RollbackResult{Path: result.Path, Branch: result.Branch}, remaining)
	assert.DirExists(result.Path)
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

func TestCreateWorktreeResultRollbackReportsBranchWhenFinalInspectionFails(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	inspectionFailure := errors.New("final branch inspection failed")
	inspectionCount := 0
	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "review/final-inspection-failure",
		Path:        filepath.Join(t.TempDir(), "inspection-failure"),
		BaseRef:     "HEAD",
		RunGit: func(
			ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
		) ([]byte, error) {
			if len(args) > 0 && args[0] == "for-each-ref" {
				inspectionCount++
				if inspectionCount == 2 {
					return nil, inspectionFailure
				}
			}
			stdout, stderr, runErr := runner.Run(ctx, dir, nil, args...)
			return append(stdout, stderr...), runErr
		},
	})
	require.NoError(err)

	remaining, err := result.Rollback(t.Context())

	require.ErrorIs(err, inspectionFailure)
	require.ErrorIs(err, ErrWorktreeCleanupIncomplete)
	assert.Equal(RollbackResult{Path: result.Path, Branch: result.Branch}, remaining)
	assert.DirExists(result.Path)
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

func TestCreateWorktreeOnDiskRunsVerifiedHookSnapshot(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	hook := filepath.Join(repo, "setup")
	trusted := []byte("#!/bin/sh\nexit 0\n")
	require.NoError(os.WriteFile(hook, trusted, 0o755))

	result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "verified-hook-snapshot",
		Path:        filepath.Join(t.TempDir(), "wt"),
		SetupScript: hook,
		RunHook: func(_ context.Context, command HookCommand) error {
			require.NoError(os.WriteFile(hook, []byte("untrusted replacement\n"), 0o755))
			executed, readErr := os.ReadFile(command.Script)
			require.NoError(readErr)
			assert.Equal(trusted, executed)
			assert.NotEqual(hook, command.Script)
			return nil
		},
	})
	require.NoError(err)
	t.Cleanup(func() { _, _ = result.Rollback(context.Background()) })
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

func TestCreateWorktreeOnDiskHookCancellationTerminatesProcessTree(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	script := filepath.Join(repo, "setup-cancel")
	require.NoError(os.WriteFile(script, []byte(
		"#!/bin/sh\nsleep 5 &\nwait\n",
	), 0o755))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	started := time.Now()

	_, err := CreateWorktreeOnDisk(ctx, CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "cancel-setup-tree",
		Path:        filepath.Join(t.TempDir(), "wt"),
		SetupScript: "setup-cancel",
	})

	require.ErrorIs(err, context.DeadlineExceeded)
	assert.Less(time.Since(started), 3*time.Second)
	var hookErr *HookError
	assert.False(errors.As(err, &hookErr), "cancellation is not a hook failure")
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

func TestRemoveWorktreeFromDiskPreservesReplacementDirectory(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	displaced := dest + "-displaced"
	lifecycleGit(t, repo, "worktree", "add", "-b", "feature", dest)
	require.NoError(os.Rename(dest, displaced))
	require.NoError(os.Mkdir(dest, 0o755))
	marker := filepath.Join(dest, "unrelated.txt")
	require.NoError(os.WriteFile(marker, []byte("preserve me\n"), 0o600))
	hookMarker := filepath.Join(t.TempDir(), "hook-ran")
	script := filepath.Join(repo, "teardown")
	require.NoError(os.WriteFile(script, []byte(
		"#!/bin/sh\nprintf ran > '"+filepath.ToSlash(hookMarker)+"'\n",
	), 0o755))

	_, err := RemoveWorktreeFromDisk(t.Context(), RemoveWorktreeOptions{
		ProjectRoot:    repo,
		Path:           dest,
		Branch:         "feature",
		Force:          true,
		RemoveBranch:   true,
		TeardownScript: "teardown",
	})

	require.ErrorIs(err, ErrWorktreeCleanupIncomplete)
	assert.FileExists(marker, "an unrelated replacement directory must be preserved")
	assert.NoFileExists(hookMarker, "the teardown hook must not run in a replacement directory")
	assert.True(branchExistsInRepo(t, repo, "feature"),
		"ownership failure must preserve the registered branch")
}

func TestRemoveDetachedWorktreePreservesNewlyAttachedCheckout(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	lifecycleGit(t, repo, "worktree", "add", "--detach", dest)
	lifecycleGit(t, dest, "switch", "-c", "newly-attached")
	hookMarker := filepath.Join(t.TempDir(), "hook-ran")
	script := filepath.Join(repo, "teardown")
	require.NoError(os.WriteFile(script, []byte(
		"#!/bin/sh\nprintf ran > '"+filepath.ToSlash(hookMarker)+"'\n",
	), 0o755))

	_, err := RemoveWorktreeFromDisk(t.Context(), RemoveWorktreeOptions{
		ProjectRoot: repo, Path: dest, TeardownScript: "teardown", Force: true,
	})

	require.ErrorIs(err, ErrWorktreeCleanupIncomplete)
	assert.DirExists(dest)
	assert.NoFileExists(hookMarker)
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

func TestRemoveMissingWorktreeSkipsMissingTeardownHook(t *testing.T) {
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	lifecycleGit(t, repo, "worktree", "add", "-b", "feature", dest)
	require.NoError(os.RemoveAll(dest))

	_, err := RemoveWorktreeFromDisk(t.Context(), RemoveWorktreeOptions{
		ProjectRoot:    repo,
		Path:           dest,
		Branch:         "feature",
		TeardownScript: "missing-teardown",
		RemoveBranch:   true,
	})

	require.NoError(err)
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

func TestRemoveWorktreeFromDiskRejectsMismatchedBranch(t *testing.T) {
	assert := assert.New(t)
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
	require.ErrorIs(err, ErrWorktreeCleanupIncomplete)
	assert.DirExists(dest, "a branch mismatch preserves the registered worktree")
	assert.True(branchExistsInRepo(t, repo, "feature"))
	assert.True(branchExistsInRepo(t, repo, "main"))
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
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	lifecycleGit(t, repo, "worktree", "add", "-b", "hidden-untracked", dest)
	lifecycleGit(t, dest, "config", "status.showUntrackedFiles", "no")
	require.NoError(os.WriteFile(filepath.Join(dest, "scratch.txt"), []byte("x\n"), 0o600))

	dirty, err := WorktreeIsDirty(t.Context(), dest)

	require.NoError(err)
	assert.True(dirty)
}

func TestWorktreeIsDirtyDisablesFiltersAndFSMonitor(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	require.NoError(os.WriteFile(
		filepath.Join(repo, ".gitattributes"), []byte("tracked.txt filter=capture\n"), 0o644,
	))
	require.NoError(os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644))
	lifecycleGit(t, repo, "add", ".gitattributes", "tracked.txt")
	lifecycleGit(t, repo, "commit", "-m", "add filtered file")

	dest := filepath.Join(t.TempDir(), "wt")
	lifecycleGit(t, repo, "worktree", "add", "-b", "isolated-status", dest)
	marker := filepath.Join(t.TempDir(), "status-command-ran")
	script := filepath.Join(t.TempDir(), "status-command.sh")
	quotedMarker := "'" + strings.ReplaceAll(filepath.ToSlash(marker), "'", "'\\''") + "'"
	require.NoError(os.WriteFile(script, []byte(
		"#!/bin/sh\nprintf ran >> "+quotedMarker+"\ncat\n",
	), 0o755))
	command := "sh '" + strings.ReplaceAll(filepath.ToSlash(script), "'", "'\\''") + "'"
	lifecycleGit(t, repo, "config", "filter.capture.clean", command)
	lifecycleGit(t, repo, "config", "core.fsmonitor", script)
	require.NoError(os.WriteFile(filepath.Join(dest, "tracked.txt"), []byte("changed\n"), 0o644))

	dirty, err := WorktreeIsDirty(t.Context(), dest)

	require.NoError(err)
	assert.True(dirty)
	assert.NoFileExists(marker)
}
