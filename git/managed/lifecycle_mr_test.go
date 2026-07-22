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

func lifecycleGitCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitenv.StripAll(os.Environ())
	return cmd
}

func identityOfCloneURL(rawURL string) string {
	return CloneURLIdentity(rawURL)
}

func TestCloneURLIdentityRecognizesSCPWithoutUsername(t *testing.T) {
	assert.Equal(t, "github.com/acme/widget",
		CloneURLIdentity("github.com:acme/widget.git"))
	assert.Equal(t, "github.com/acme/widget",
		CloneURLIdentity("git@github.com:acme/widget.git"))
	assert.Equal(t, `C:\repos\widget.git`,
		CloneURLIdentity(`C:\repos\widget.git`))
	assert.Equal(t, "./local:path/widget.git",
		CloneURLIdentity("./local:path/widget.git"))
}

// initOriginAndClone builds an "origin" repository with one commit on main
// and a clone of it, returning (originDir, cloneDir). The clone is the
// project checkout merge-request worktrees are created in.
func initOriginAndClone(t *testing.T) (string, string) {
	t.Helper()
	origin := initLifecycleRepo(t)
	clone := filepath.Join(t.TempDir(), "clone")
	lifecycleGit(t, filepath.Dir(origin), "clone", "-q", origin, clone)
	configureLifecycleGitIdentity(t, clone)
	return origin, clone
}

func worktreeConfig(t *testing.T, dir, key string) string {
	t.Helper()
	cmd := lifecycleGitCommand(dir, "config", "--get", key)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TestCreateWorktreeFromMergeRequestSameRepo covers the same-repo scenario:
// the head branch is fetched from origin, the new local branch starts at
// it, and upstream tracking points at origin's head branch.
func TestCreateWorktreeFromMergeRequestSameRepo(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "feature-x")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "pr work")
	headSHA := lifecycleGit(t, origin, "rev-parse", "feature-x")
	lifecycleGit(t, origin, "checkout", "-q", "main")

	dest := filepath.Join(t.TempDir(), "wt")
	result, err := CreateWorktreeFromMergeRequest(
		context.Background(), MergeRequestWorktreeOptions{
			ProjectRoot:         clone,
			Branch:              "pr-42",
			Path:                dest,
			Number:              42,
			HeadBranch:          "feature-x",
			HeadRepoCloneURL:    origin,
			ProjectRepoIdentity: identityOfCloneURL(origin),
		})
	require.NoError(err)
	assert.Equal(dest, result.Path)
	assert.Equal(headSHA, lifecycleGit(t, dest, "rev-parse", "HEAD"),
		"worktree starts at the merge request head")
	assert.Equal("pr-42",
		lifecycleGit(t, dest, "rev-parse", "--abbrev-ref", "HEAD"))
	assert.Equal("origin", worktreeConfig(t, dest, "branch.pr-42.remote"))
	assert.Equal("refs/heads/feature-x",
		worktreeConfig(t, dest, "branch.pr-42.merge"))
	assert.Equal("upstream", worktreeConfig(t, dest, "push.default"))
}

func TestPrepareMergeRequestRemoteKeepsCaseDistinctLocalRepositoriesSeparate(t *testing.T) {
	repo := initLifecycleRepo(t)
	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{
		ProjectRoot:     repo,
		ExpectedHeadOID: strings.Repeat("a", 40),
	})
	Require.NoError(t, err)
	base := t.TempDir()

	target, err := prepareMergeRequestRemote(t.Context(), backend, MergeRequestWorktreeOptions{
		Number:              18,
		Platform:            "github",
		HeadBranch:          "feature",
		HeadRepoCloneURL:    filepath.Join(base, "CaseRepo"),
		ProjectRepoIdentity: filepath.Join(base, "caserepo"),
	})

	Require.NoError(t, err)
	assert.Equal(t, "refs/pull/18/head", target.checkoutSourceRef)
	assert.NotEqual(t, "origin", target.trackingRemote)
}

// TestCreateWorktreeFromMergeRequestPullRefFallback covers the no-fork-URL
// scenario: the merge request head is fetched via the platform pull ref and
// no upstream tracking is configured.
func TestCreateWorktreeFromMergeRequestPullRefFallback(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "contributor-work")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "pr work")
	headSHA := lifecycleGit(t, origin, "rev-parse", "contributor-work")
	lifecycleGit(t, origin, "update-ref", "refs/pull/7/head", headSHA)
	lifecycleGit(t, origin, "checkout", "-q", "main")

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(
		context.Background(), MergeRequestWorktreeOptions{
			ProjectRoot: clone,
			Branch:      "pr-7",
			Path:        dest,
			Number:      7,
			HeadBranch:  "contributor-work",
		})
	require.NoError(err)
	assert.Equal(headSHA, lifecycleGit(t, dest, "rev-parse", "HEAD"),
		"worktree starts at the pull ref head")
	assert.Empty(worktreeConfig(t, dest, "branch.pr-7.remote"),
		"no tracking without a fork clone URL")
}

func TestCreateWorktreeFromMergeRequestSameRepoWithoutHeadBranchUsesPullRef(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "unnamed-head")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "pr work")
	headSHA := lifecycleGit(t, origin, "rev-parse", "unnamed-head")
	lifecycleGit(t, origin, "update-ref", "refs/pull/12/head", headSHA)
	lifecycleGit(t, origin, "checkout", "-q", "main")

	result, err := CreateWorktreeFromMergeRequest(t.Context(), MergeRequestWorktreeOptions{
		ProjectRoot:         clone,
		Branch:              "pr-12",
		Path:                filepath.Join(t.TempDir(), "wt"),
		Number:              12,
		HeadRepoCloneURL:    origin,
		ProjectRepoIdentity: identityOfCloneURL(origin),
	})

	require.NoError(err)
	t.Cleanup(func() { _, _ = result.Rollback(context.Background()) })
	assert.Equal(headSHA, lifecycleGit(t, result.Path, "rev-parse", "HEAD"))
	assert.Empty(worktreeConfig(t, result.Path, "branch.pr-12.remote"))
}

func TestCreateWorktreeFromForkWithoutHeadBranchDisablesTracking(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	origin, clone := initOriginAndClone(t)
	fork := filepath.Join(t.TempDir(), "fork")
	lifecycleGit(t, filepath.Dir(origin), "clone", "-q", origin, fork)
	configureLifecycleGitIdentity(t, fork)
	lifecycleGit(t, fork, "checkout", "-q", "-b", "merge-request")
	lifecycleGit(t, fork, "commit", "--allow-empty", "-m", "unnamed fork head")
	headSHA := lifecycleGit(t, fork, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "fetch", "-q", fork,
		"+refs/heads/merge-request:refs/pull/13/head")

	result, err := CreateWorktreeFromMergeRequest(t.Context(), MergeRequestWorktreeOptions{
		ProjectRoot:         clone,
		Branch:              "pr-13",
		Path:                filepath.Join(t.TempDir(), "wt"),
		Number:              13,
		HeadRepoCloneURL:    fork,
		ProjectRepoIdentity: identityOfCloneURL(origin),
	})

	require.NoError(err)
	t.Cleanup(func() { _, _ = result.Rollback(context.Background()) })
	assert.Equal(headSHA, lifecycleGit(t, result.Path, "rev-parse", "HEAD"))
	assert.Empty(worktreeConfig(t, result.Path, "branch.pr-13.remote"))
}

func TestCreateWorktreeFromMergeRequestCanonicalizesRelativeForkPath(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	origin, clone := initOriginAndClone(t)
	fork := filepath.Join(filepath.Dir(clone), "forks", "team", "fork")
	require.NoError(os.MkdirAll(filepath.Dir(fork), 0o755))
	lifecycleGit(t, filepath.Dir(fork), "clone", "-q", origin, fork)
	configureLifecycleGitIdentity(t, fork)
	lifecycleGit(t, fork, "checkout", "-q", "-b", "relative-fork")
	lifecycleGit(t, fork, "commit", "--allow-empty", "-m", "relative fork head")
	headSHA := lifecycleGit(t, fork, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "fetch", "-q", fork,
		"+refs/heads/relative-fork:refs/pull/14/head")
	relativeFork, err := filepath.Rel(clone, fork)
	require.NoError(err)

	result, err := CreateWorktreeFromMergeRequest(t.Context(), MergeRequestWorktreeOptions{
		ProjectRoot:         clone,
		Branch:              "pr-14",
		Path:                filepath.Join(t.TempDir(), "wt"),
		Number:              14,
		HeadBranch:          "relative-fork",
		HeadRepoCloneURL:    relativeFork,
		ProjectRepoIdentity: identityOfCloneURL(origin),
	})

	require.NoError(err)
	t.Cleanup(func() { _, _ = result.Rollback(context.Background()) })
	assert.Equal(headSHA, lifecycleGit(t, result.Path, "rev-parse", "HEAD"))
	remote := worktreeConfig(t, result.Path, "branch.pr-14.remote")
	assert.NotEmpty(remote)
	assert.Equal(fork, lifecycleGit(t, clone, "remote", "get-url", remote))
}

func TestCreateWorktreeFromMergeRequestChecksOutVerifiedOID(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "contributor-work")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "pr work")
	headSHA := lifecycleGit(t, origin, "rev-parse", "contributor-work")
	lifecycleGit(t, origin, "update-ref", "refs/pull/8/head", headSHA)
	lifecycleGit(t, origin, "checkout", "-q", "main")
	mainSHA := lifecycleGit(t, origin, "rev-parse", "main")
	mutated := false

	result, err := CreateWorktreeFromMergeRequest(t.Context(), MergeRequestWorktreeOptions{
		ProjectRoot:     clone,
		Branch:          "pr-8",
		Path:            filepath.Join(t.TempDir(), "wt"),
		Number:          8,
		HeadBranch:      "contributor-work",
		ExpectedHeadSHA: headSHA,
		RunGit: func(
			ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
		) ([]byte, error) {
			if !mutated && len(args) >= 2 && args[0] == "worktree" && args[1] == "add" {
				mutated = true
				lifecycleGit(t, clone, "update-ref", "refs/remotes/origin/pull/8/head", mainSHA)
			}
			stdout, stderr, runErr := runner.Run(ctx, dir, nil, args...)
			return append(stdout, stderr...), runErr
		},
	})
	require.NoError(err)
	t.Cleanup(func() { _, _ = result.Rollback(context.Background()) })
	assert.True(mutated)
	assert.Equal(headSHA, lifecycleGit(t, result.Path, "rev-parse", "HEAD"))
}

// TestCreateWorktreeFromMergeRequestFork covers the fork scenario: checkout
// still comes from origin's pull ref, while tracking is configured against
// a dedicated fork remote.
func TestCreateWorktreeFromMergeRequestFork(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)

	// The fork: a second clone of origin with the head branch.
	fork := filepath.Join(t.TempDir(), "fork")
	lifecycleGit(t, filepath.Dir(origin), "clone", "-q", origin, fork)
	lifecycleGit(t, fork, "config", "user.email", "t@e.st")
	lifecycleGit(t, fork, "config", "user.name", "Tester")
	lifecycleGit(t, fork, "checkout", "-q", "-b", "fork-work")
	lifecycleGit(t, fork, "commit", "--allow-empty", "-m", "fork pr work")
	headSHA := lifecycleGit(t, fork, "rev-parse", "fork-work")

	// GitHub exposes the fork's head on origin's pull ref.
	lifecycleGit(t, origin, "fetch", "-q", fork,
		"+refs/heads/fork-work:refs/pull/9/head")

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(
		context.Background(), MergeRequestWorktreeOptions{
			ProjectRoot:         clone,
			Branch:              "pr-9",
			Path:                dest,
			Number:              9,
			HeadBranch:          "fork-work",
			HeadRepoCloneURL:    fork,
			ProjectRepoIdentity: identityOfCloneURL(origin),
		})
	require.NoError(err)
	assert.Equal(headSHA, lifecycleGit(t, dest, "rev-parse", "HEAD"))
	remote := worktreeConfig(t, dest, "branch.pr-9.remote")
	assert.NotEmpty(remote, "fork tracking remote configured")
	assert.NotEqual("origin", remote)
	assert.Equal("refs/heads/fork-work",
		worktreeConfig(t, dest, "branch.pr-9.merge"))
	remoteURL := lifecycleGit(t, clone, "remote", "get-url", remote)
	assert.Equal(fork, remoteURL)
}

func TestCreateWorktreeFromMergeRequestDisablesTrackingWhenForkHeadDiffers(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	origin, clone := initOriginAndClone(t)
	fork := filepath.Join(t.TempDir(), "fork")
	lifecycleGit(t, filepath.Dir(origin), "clone", "-q", origin, fork)
	lifecycleGit(t, fork, "config", "user.email", "t@e.st")
	lifecycleGit(t, fork, "config", "user.name", "Tester")
	lifecycleGit(t, fork, "checkout", "-q", "-b", "fork-work")
	lifecycleGit(t, fork, "commit", "--allow-empty", "-m", "selected pr work")
	selectedSHA := lifecycleGit(t, fork, "rev-parse", "fork-work")
	lifecycleGit(t, origin, "fetch", "-q", fork,
		"+refs/heads/fork-work:refs/pull/10/head")
	lifecycleGit(t, fork, "commit", "--allow-empty", "-m", "later fork work")

	result, err := CreateWorktreeFromMergeRequest(t.Context(), MergeRequestWorktreeOptions{
		ProjectRoot:         clone,
		Branch:              "pr-10",
		Path:                filepath.Join(t.TempDir(), "wt"),
		Number:              10,
		HeadBranch:          "fork-work",
		HeadRepoCloneURL:    fork,
		ExpectedHeadSHA:     selectedSHA,
		ProjectRepoIdentity: identityOfCloneURL(origin),
	})
	require.NoError(err)
	t.Cleanup(func() { _, _ = result.Rollback(context.Background()) })
	assert.Equal(selectedSHA, lifecycleGit(t, result.Path, "rev-parse", "HEAD"))
	assert.Empty(worktreeConfig(t, result.Path, "branch.pr-10.remote"))
}

// TestCreateWorktreeFromMergeRequestTrackingFetchFailureIsNonFatal: when
// the fork cannot be fetched, the import still succeeds via the pull ref
// and tracking is silently disabled.
func TestCreateWorktreeFromMergeRequestTrackingFetchFailureIsNonFatal(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "gone-fork-work")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "pr work")
	headSHA := lifecycleGit(t, origin, "rev-parse", "gone-fork-work")
	lifecycleGit(t, origin, "update-ref", "refs/pull/11/head", headSHA)
	lifecycleGit(t, origin, "checkout", "-q", "main")

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(
		context.Background(), MergeRequestWorktreeOptions{
			ProjectRoot:         clone,
			Branch:              "pr-11",
			Path:                dest,
			Number:              11,
			HeadBranch:          "gone-fork-work",
			HeadRepoCloneURL:    filepath.Join(t.TempDir(), "no-such-fork"),
			ProjectRepoIdentity: identityOfCloneURL(origin),
		})
	require.NoError(err)
	assert.Equal(headSHA, lifecycleGit(t, dest, "rev-parse", "HEAD"))
	assert.Empty(worktreeConfig(t, dest, "branch.pr-11.remote"),
		"tracking silently disabled when the fork fetch fails")
}

// TestCreateWorktreeFromMergeRequestHookFailureRollsBack: a failing setup
// hook rolls back the imported worktree and its branch.
func TestCreateWorktreeFromMergeRequestHookFailureRollsBack(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "feature-x")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "pr work")
	lifecycleGit(t, origin, "checkout", "-q", "main")
	require.NoError(os.WriteFile(
		filepath.Join(clone, "setup"),
		[]byte("#!/bin/sh\nprintf partial > setup-output.txt\nexit 5\n"), 0o755))

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(
		context.Background(), MergeRequestWorktreeOptions{
			ProjectRoot:         clone,
			Branch:              "pr-42",
			Path:                dest,
			Number:              42,
			HeadBranch:          "feature-x",
			HeadRepoCloneURL:    origin,
			ProjectRepoIdentity: identityOfCloneURL(origin),
			SetupScript:         "setup",
		})
	var hookErr *HookError
	require.ErrorAs(err, &hookErr)
	assert.Equal(5, hookErr.ExitCode)
	_, statErr := os.Stat(dest)
	assert.True(os.IsNotExist(statErr))
	assert.False(branchExistsInRepo(t, clone, "pr-42"))
}

func TestCreateWorktreeFromMergeRequestPersistsSafePushRouting(t *testing.T) {
	origin, clone := initOriginAndClone(t)
	fork := filepath.Join(t.TempDir(), "fork")
	lifecycleGit(t, clone, "clone", origin, fork)
	lifecycleGit(t, fork, "config", "user.email", "t@e.st")
	lifecycleGit(t, fork, "config", "user.name", "Tester")
	lifecycleGit(t, fork, "checkout", "-b", "feature-safe")
	lifecycleGit(t, fork, "commit", "--allow-empty", "-m", "safe routing")
	lifecycleGit(t, origin, "fetch", fork, "+refs/heads/feature-safe:refs/pull/51/head")
	lifecycleGit(t, clone, "config", "core.hooksPath", ".githooks")

	result, err := CreateWorktreeFromMergeRequest(context.Background(), MergeRequestWorktreeOptions{
		ProjectRoot:         clone,
		Branch:              "mr-51-safe-routing",
		BaseDir:             t.TempDir(),
		Number:              51,
		HeadBranch:          "feature-safe",
		HeadRepoCloneURL:    fork,
		ProjectRepoIdentity: identityOfCloneURL(origin),
		Platform:            "github",
	})
	Require.NoError(t, err)
	t.Cleanup(func() { _, _ = result.Rollback(context.Background()) })

	hooksPath := lifecycleGit(t, result.Path, "config", "--path", "--get", "core.hooksPath")
	assert.True(t, filepath.IsAbs(hooksPath), hooksPath)
	assert.DirExists(t, hooksPath)
	assert.Equal(t, "upstream", worktreeConfig(t, result.Path, "push.default"))
}

func TestCreateWorktreeFromMergeRequestRejectsChangedHead(t *testing.T) {
	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "feature-changed")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "changed head")
	lifecycleGit(t, origin, "checkout", "-q", "main")

	destination := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(context.Background(), MergeRequestWorktreeOptions{
		ProjectRoot:         clone,
		Branch:              "mr-52-changed",
		Path:                destination,
		Number:              52,
		HeadBranch:          "feature-changed",
		HeadRepoCloneURL:    origin,
		ProjectRepoIdentity: identityOfCloneURL(origin),
		ExpectedHeadSHA:     strings.Repeat("0", 40),
	})

	var changeRequestErr *ChangeRequestError
	Require.ErrorAs(t, err, &changeRequestErr)
	assert.Equal(t, ChangeRequestHeadChanged, changeRequestErr.Kind)
	assert.NoDirExists(t, destination)
}
