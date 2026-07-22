package managedworktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
	gitremote "go.kenn.io/kit/git/remote"
)

func newChangeRequestGit(t *testing.T) (string, *ChangeRequestGit) {
	t.Helper()
	repo := initLifecycleRepo(t)
	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{
		ProjectRoot:            repo,
		ProjectIdentity:        gitremote.Identity{Host: "github.com", Owner: "acme", Name: "widget"},
		RemoteNamePrefix:       "review",
		HookIsolationNamespace: "kit-test",
		Runner:                 gitcmd.New(),
	})
	require.NoError(t, err)
	return repo, backend
}

func changeRequestRemote(owner string) RemoteRepository {
	return RemoteRepository{
		Identity: gitremote.Identity{Host: "github.com", Owner: owner, Name: "widget"},
		CloneURL: "https://github.com/" + owner + "/widget.git",
	}
}

func TestChangeRequestGitValidateRejectsUnsafeEffectiveConfiguration(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, repo string)
		kind  ChangeRequestErrorKind
	}{
		{
			name: "credential URL",
			setup: func(t *testing.T, repo string) {
				lifecycleGit(t, repo, "remote", "add", "origin", "https://oauth2:secret@github.com/acme/widget.git")
			},
			kind: ChangeRequestAuthentication,
		},
		{
			name: "remote helper rewrite",
			setup: func(t *testing.T, repo string) {
				lifecycleGit(t, repo, "remote", "add", "origin", "https://alias/acme/widget.git")
				lifecycleGit(t, repo, "config", "url.corp::--token=secret.insteadOf", "https://alias/")
			},
			kind: ChangeRequestAuthentication,
		},
		{
			name: "custom receive pack",
			setup: func(t *testing.T, repo string) {
				lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
				lifecycleGit(t, repo, "config", "remote.origin.receivepack", "sh -c redirect")
			},
			kind: ChangeRequestUnsafeConfiguration,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo, backend := newChangeRequestGit(t)
			test.setup(t, repo)

			err := backend.Validate(context.Background())

			var typed *ChangeRequestError
			require.ErrorAs(t, err, &typed)
			assert.Equal(t, test.kind, typed.Kind)
			assert.NotContains(t, err.Error(), "secret")
			assert.NotContains(t, err.Error(), "redirect")
		})
	}
}

func TestChangeRequestGitEnsureRemoteRejectsEffectiveURLRewrite(t *testing.T) {
	repo, backend := newChangeRequestGit(t)
	lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
	lifecycleGit(t, repo, "config", "url.https://github.com/attacker/.insteadOf", "https://github.com/octocat/")

	_, err := backend.EnsureRemote(context.Background(), changeRequestRemote("octocat"))

	var typed *ChangeRequestError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, ChangeRequestUnsafeConfiguration, typed.Kind)
	assert.NotContains(t, strings.Fields(lifecycleGit(t, repo, "remote")), "review-octocat")
}

func TestChangeRequestGitEnsureRemotePreservesProjectSSHTransport(t *testing.T) {
	repo, backend := newChangeRequestGit(t)
	lifecycleGit(t, repo, "remote", "add", "origin", "ssh://custom@github.com:2222/acme/widget.git")

	remote, err := backend.EnsureRemote(context.Background(), changeRequestRemote("octocat"))

	require.NoError(t, err)
	assert.Equal(t, "review-octocat", remote)
	assert.Equal(t, "ssh://custom@github.com:2222/octocat/widget.git",
		lifecycleGit(t, repo, "remote", "get-url", remote))
}

func TestChangeRequestGitConfiguresPersistentSafePushRouting(t *testing.T) {
	repo, backend := newChangeRequestGit(t)
	lifecycleGit(t, repo, "remote", "add", "fork", "https://github.com/octocat/widget.git")
	created, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Path:        filepath.Join(t.TempDir(), "review-worktree"),
		Branch:      "review-8-feature",
		BaseRef:     "main",
		Runner:      gitcmd.New(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = created.Rollback(context.Background()) })

	err = backend.ConfigurePush(context.Background(), created, "fork", changeRequestRemote("octocat"), "feature/widgets")

	require.NoError(t, err)
	assert.Equal(t, "fork", lifecycleGit(t, created.Path, "config", "--worktree", "--get", "branch.review-8-feature.pushRemote"))
	assert.Equal(t, "HEAD:refs/heads/feature/widgets", lifecycleGit(t, created.Path, "config", "--worktree", "--get", "remote.fork.push"))
	hooksPath := lifecycleGit(t, created.Path, "config", "--path", "--get", "core.hooksPath")
	assert.True(t, filepath.IsAbs(hooksPath), hooksPath)
	assert.DirExists(t, hooksPath)
}

func TestChangeRequestGitConfigurePushRejectsChangedHead(t *testing.T) {
	repo, backend := newChangeRequestGit(t)
	lifecycleGit(t, repo, "remote", "add", "fork", "https://github.com/octocat/widget.git")
	created, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Path:        filepath.Join(t.TempDir(), "review-worktree"),
		Branch:      "review-9-feature",
		BaseRef:     "main",
		Runner:      gitcmd.New(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = created.Rollback(context.Background()) })
	lifecycleGit(t, created.Path, "checkout", "--detach", "HEAD")

	err = backend.ConfigurePush(context.Background(), created, "fork", changeRequestRemote("octocat"), "feature/widgets")

	var typed *ChangeRequestError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, ChangeRequestUnsafeConfiguration, typed.Kind)
}

func TestChangeRequestGitFetchDisablesRepositoryHooks(t *testing.T) {
	repo, backend := newChangeRequestGit(t)
	bare := filepath.Join(t.TempDir(), "fork.git")
	lifecycleGit(t, repo, "init", "--bare", bare)
	lifecycleGit(t, repo, "push", bare, "HEAD:refs/heads/topic")
	lifecycleGit(t, repo, "remote", "add", "fork", bare)
	marker := filepath.Join(t.TempDir(), "reference-transaction-ran")
	hook := filepath.Join(repo, ".git", "hooks", "reference-transaction")
	quotedMarker := "'" + strings.ReplaceAll(filepath.ToSlash(marker), "'", "'\\''") + "'"
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nprintf ran > "+quotedMarker+"\n"), 0o755))
	head := lifecycleGit(t, repo, "rev-parse", "HEAD")
	lifecycleGit(t, repo, "update-ref", "refs/kit/hook-probe", head)
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		t.Skip("installed Git does not support reference-transaction hooks")
	}
	require.NoError(t, os.Remove(marker))

	_, err := backend.Fetch(context.Background(), "fork", "refs/heads/topic", "refs/kit/reviews/1")

	require.NoError(t, err)
	assert.NoFileExists(t, marker)
}

func TestChangeRequestGitNormalPushCannotRunContributorHook(t *testing.T) {
	repo, backend := newChangeRequestGit(t)
	marker := filepath.Join(t.TempDir(), "pre-push-ran")
	hooksDir := filepath.Join(repo, ".githooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0o755))
	quotedMarker := "'" + strings.ReplaceAll(filepath.ToSlash(marker), "'", "'\\''") + "'"
	require.NoError(t, os.WriteFile(filepath.Join(hooksDir, "pre-push"),
		[]byte("#!/bin/sh\nprintf ran > "+quotedMarker+"\n"), 0o755))
	lifecycleGit(t, repo, "add", ".githooks/pre-push")
	lifecycleGit(t, repo, "commit", "-m", "add contributor hook")
	lifecycleGit(t, repo, "config", "core.hooksPath", ".githooks")
	bare := filepath.Join(t.TempDir(), "fork.git")
	lifecycleGit(t, repo, "init", "--bare", bare)
	lifecycleGit(t, repo, "remote", "add", "fork", bare)
	created, err := CreateWorktreeOnDisk(context.Background(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Path:        filepath.Join(t.TempDir(), "review-worktree"),
		Branch:      "review-10-safe-push",
		BaseRef:     "main",
		Runner:      gitcmd.New(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = created.Rollback(context.Background()) })
	localRemote := RemoteRepository{
		Identity: gitremote.Identity{Host: "", Owner: "", Name: ""},
		CloneURL: bare,
	}
	require.NoError(t, backend.ConfigurePush(context.Background(), created, "fork", localRemote, "topic"))

	cmd := exec.Command("git", "push", "fork")
	cmd.Dir = created.Path
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.NoFileExists(t, marker)
}
