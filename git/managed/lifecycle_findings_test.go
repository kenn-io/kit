package managedworktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
)

// TestCreateWorktreeOnDiskReportsBranchCreated pins the BranchCreated flag:
// rollback may delete a branch this call created, but never a pre-existing one.
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
	assert.False(attached.BranchCreated,
		"attaching a pre-existing branch must not report it as created")

	created, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "brand-new",
		Path:        filepath.Join(t.TempDir(), "wt-new"),
	})
	require.NoError(err)
	assert.True(created.BranchCreated)
}

// TestCreateWorktreeResultRollbackPreservesPreexistingBranch covers the
// registry-conflict rollback path: a conservative result rollback removes the
// unchanged worktree but leaves a branch the operation did not create.
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
	assert.True(os.IsNotExist(statErr), "rollback must remove the worktree")
	assert.True(branchExistsInRepo(t, repo, "keep-me"),
		"rollback must not delete a branch it did not create")
}

// TestCreateWorktreeOnDiskRejectsSymlinkedHookEscape verifies hook confinement
// resolves symlinks: a symlink inside the project pointing at a script outside
// it must be rejected, not executed.
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

func TestCreateWorktreeOnDiskRejectsDanglingHookBeforeMutation(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	dest := filepath.Join(t.TempDir(), "future-worktree")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	quotedMarker := "'" + strings.ReplaceAll(filepath.ToSlash(marker), "'", "'\\''") + "'"
	require.NoError(os.WriteFile(
		filepath.Join(repo, "setup"),
		[]byte("#!/bin/sh\nprintf ran > "+quotedMarker+"\n"), 0o755,
	))
	lifecycleGit(t, repo, "add", "setup")
	lifecycleGit(t, repo, "commit", "-m", "add contributor setup")
	require.NoError(os.Symlink(filepath.Join(dest, "setup"), filepath.Join(repo, "hook-link")))

	_, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "dangling-hook",
		Path:        dest,
		SetupScript: "hook-link",
	})

	require.ErrorContains(err, "resolve lifecycle hook script")
	assert.NoDirExists(dest)
	assert.False(branchExistsInRepo(t, repo, "dangling-hook"))
	assert.NoFileExists(marker)
}

func TestCreateWorktreeOnDiskRevalidatesHookIdentityBeforeExecution(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	repo := initLifecycleRepo(t)
	hook := filepath.Join(repo, "setup")
	require.NoError(os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	marker := filepath.Join(t.TempDir(), "replacement-ran")
	quotedMarker := "'" + strings.ReplaceAll(filepath.ToSlash(marker), "'", "'\\''") + "'"
	dest := filepath.Join(t.TempDir(), "worktree")

	_, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot:      repo,
		Branch:           "replaced-hook",
		Path:             dest,
		SetupScript:      "setup",
		IsolatedCheckout: true,
		BeforeCheckout: func(context.Context, string) error {
			require.NoError(os.Remove(hook))
			return os.WriteFile(hook, []byte(
				"#!/bin/sh\nprintf ran > "+quotedMarker+"\n",
			), 0o755)
		},
	})

	require.Error(err)
	assert.NoFileExists(marker)
}

// TestCreateWorktreeFromMergeRequestGitLabRef verifies the merge-request head
// resolution is provider-aware: a GitLab project fetches
// refs/merge-requests/<n>/head instead of GitHub's refs/pull/<n>/head.
func TestCreateWorktreeFromMergeRequestGitLabRef(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)

	origin, clone := initOriginAndClone(t)
	lifecycleGit(t, origin, "checkout", "-q", "-b", "mr-work")
	lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "mr work")
	headSHA := lifecycleGit(t, origin, "rev-parse", "mr-work")
	lifecycleGit(t, origin, "update-ref", "refs/merge-requests/5/head", headSHA)
	lifecycleGit(t, origin, "checkout", "-q", "main")

	dest := filepath.Join(t.TempDir(), "wt")
	_, err := CreateWorktreeFromMergeRequest(
		context.Background(), MergeRequestWorktreeOptions{
			ProjectRoot: clone,
			Branch:      "mr-5",
			Path:        dest,
			Number:      5,
			HeadBranch:  "mr-work",
			Platform:    "gitlab",
		})
	require.NoError(err)
	assert.Equal(headSHA, lifecycleGit(t, dest, "rev-parse", "HEAD"),
		"worktree starts at the GitLab merge-request ref head")
}
