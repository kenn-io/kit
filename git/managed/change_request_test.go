package managedworktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
	gitremote "go.kenn.io/kit/git/remote"
	"go.kenn.io/kit/safefileio"
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
			name: "plaintext hosted transport",
			setup: func(t *testing.T, repo string) {
				lifecycleGit(t, repo, "remote", "add", "origin", "http://github.com/acme/widget.git")
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
		{
			name: "custom upload pack",
			setup: func(t *testing.T, repo string) {
				lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
				lifecycleGit(t, repo, "config", "remote.origin.uploadpack", "sh -c fetch-redirect")
			},
			kind: ChangeRequestUnsafeConfiguration,
		},
		{
			name: "custom remote helper",
			setup: func(t *testing.T, repo string) {
				lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
				lifecycleGit(t, repo, "config", "remote.origin.vcs", "evil")
			},
			kind: ChangeRequestUnsafeConfiguration,
		},
		{
			name: "custom git proxy",
			setup: func(t *testing.T, repo string) {
				lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
				lifecycleGit(t, repo, "config", "core.gitProxy", "sh -c proxy")
			},
			kind: ChangeRequestUnsafeConfiguration,
		},
		{
			name: "custom ssh command",
			setup: func(t *testing.T, repo string) {
				lifecycleGit(t, repo, "remote", "add", "origin", "ssh://git@github.com/acme/widget.git")
				lifecycleGit(t, repo, "config", "core.sshCommand", "sh -c ssh-redirect")
			},
			kind: ChangeRequestUnsafeConfiguration,
		},
		{
			name: "authorization header",
			setup: func(t *testing.T, repo string) {
				lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
				lifecycleGit(t, repo, "config", "http.extraHeader", "Authorization: Bearer secret")
			},
			kind: ChangeRequestAuthentication,
		},
		{
			name: "cookie file",
			setup: func(t *testing.T, repo string) {
				lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
				lifecycleGit(t, repo, "config", "http.cookieFile", filepath.Join(t.TempDir(), "cookies"))
			},
			kind: ChangeRequestAuthentication,
		},
		{
			name: "disabled TLS verification",
			setup: func(t *testing.T, repo string) {
				lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
				lifecycleGit(t, repo, "config", "http.https://github.com/.sslVerify", "false")
			},
			kind: ChangeRequestAuthentication,
		},
		{
			name: "client certificate",
			setup: func(t *testing.T, repo string) {
				lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
				lifecycleGit(t, repo, "config", "http.sslCert", filepath.Join(t.TempDir(), "client.pem"))
			},
			kind: ChangeRequestAuthentication,
		},
		{
			name: "credential-bearing proxy",
			setup: func(t *testing.T, repo string) {
				lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
				lifecycleGit(t, repo, "config", "http.proxy", "https://user:secret@proxy.example")
			},
			kind: ChangeRequestAuthentication,
		},
		{
			name: "remote-scoped proxy",
			setup: func(t *testing.T, repo string) {
				lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
				lifecycleGit(t, repo, "config", "remote.origin.proxy", "https://user:secret@proxy.example")
			},
			kind: ChangeRequestAuthentication,
		},
		{
			name: "remote-scoped proxy authentication",
			setup: func(t *testing.T, repo string) {
				lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
				lifecycleGit(t, repo, "config", "remote.origin.proxyAuthMethod", "basic")
			},
			kind: ChangeRequestAuthentication,
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

func TestNewChangeRequestGitPreservesConfiguredRunnerWithNilEnvironment(t *testing.T) {
	repo := initLifecycleRepo(t)
	runner := gitcmd.Runner{Config: []gitcmd.Config{{Key: "gc.auto", Value: "0"}}}

	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{
		ProjectRoot: repo,
		Runner:      runner,
	})

	require.NoError(t, err)
	assert.Equal(t, runner.Config, backend.runner.Config)
	assert.NotNil(t, backend.runner.Env)
}

func TestChangeRequestGitForcesNonInteractiveRunner(t *testing.T) {
	repo := initLifecycleRepo(t)
	lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
	runner := gitcmd.New()
	runner.TerminalPrompt = true
	commands := 0
	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{
		ProjectRoot: repo,
		ProjectIdentity: gitremote.Identity{
			Host: "github.com", Owner: "acme", Name: "widget",
		},
		Runner: runner,
		RunGit: func(
			ctx context.Context, effective gitcmd.Runner, dir string, args ...string,
		) ([]byte, error) {
			commands++
			assert.False(t, effective.TerminalPrompt,
				"change-request Git commands must remain noninteractive")
			stdout, stderr, runErr := effective.Run(ctx, dir, nil, args...)
			return append(stdout, stderr...), runErr
		},
	})
	require.NoError(t, err)

	require.NoError(t, backend.Validate(t.Context()))
	assert.Positive(t, commands)
}

func TestChangeRequestGitValidateRejectsHostedOriginWithoutTrustAnchor(t *testing.T) {
	repo := initLifecycleRepo(t)
	lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{ProjectRoot: repo})
	require.NoError(t, err)

	err = backend.Validate(t.Context())

	var typed *ChangeRequestError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, ChangeRequestUnsafeConfiguration, typed.Kind)
}

func TestChangeRequestGitValidateAllowsHostedOriginWithExpectedHead(t *testing.T) {
	repo := initLifecycleRepo(t)
	lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{
		ProjectRoot:     repo,
		ExpectedHeadOID: strings.Repeat("a", 40),
	})
	require.NoError(t, err)

	assert.NoError(t, backend.Validate(t.Context()))
}

func TestChangeRequestWorktreeConfigVersionRequirement(t *testing.T) {
	for _, test := range []struct {
		output string
		goos   string
		want   bool
	}{
		{output: "git version 2.20.0", goos: "linux"},
		{output: "git version 2.39.0", goos: "linux"},
		{output: "git version 2.39.1", goos: "linux", want: true},
		{output: "git version 2.45.2 (Apple Git-145)", goos: "darwin", want: true},
		{output: "git version 2.52.2.windows.4", goos: "windows"},
		{output: "git version 2.53.0", goos: "windows"},
		{output: "git version 2.53.0.windows.2", goos: "windows"},
		{output: "git version 2.53.0.windows.3-malformed", goos: "windows"},
		{output: "git version 2.53.0.windows.3", goos: "windows", want: true},
		{output: "git version 2.53.1.windows.1", goos: "windows", want: true},
		{output: "git version 2.54.0.windows.1", goos: "windows", want: true},
		{output: "not git", goos: "linux"},
	} {
		assert.Equal(t, test.want,
			supportsChangeRequestGitVersion(test.output, test.goos), test.output)
	}
}

func TestChangeRequestGitFetchEnforcesConfiguredExpectedHead(t *testing.T) {
	repo := initLifecycleRepo(t)
	bare := filepath.Join(t.TempDir(), "origin.git")
	lifecycleGit(t, repo, "init", "--bare", bare)
	originalOID := lifecycleGit(t, repo, "rev-parse", "HEAD")
	lifecycleGit(t, repo, "commit", "--allow-empty", "-m", "remote head")
	lifecycleGit(t, repo, "push", bare, "HEAD:refs/heads/topic")
	lifecycleGit(t, repo, "remote", "add", "origin", bare)
	lifecycleGit(t, repo, "update-ref", "refs/kit/merge-requests/1/head", originalOID)
	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{
		ProjectRoot:     repo,
		ExpectedHeadOID: strings.Repeat("a", 40),
	})
	require.NoError(t, err)
	require.NoError(t, backend.Validate(t.Context()))

	_, err = backend.Fetch(
		t.Context(), "origin", "refs/heads/topic", "refs/kit/merge-requests/1/head",
	)

	var typed *ChangeRequestError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, ChangeRequestHeadChanged, typed.Kind)
	assert.Equal(t, originalOID,
		lifecycleGit(t, repo, "rev-parse", "refs/kit/merge-requests/1/head"),
		"an unverified fetch must not publish over the destination ref")
}

func TestChangeRequestGitFetchExpectedValidatesBeforePublishing(t *testing.T) {
	repo := initLifecycleRepo(t)
	bare := filepath.Join(t.TempDir(), "origin.git")
	lifecycleGit(t, repo, "init", "--bare", bare)
	originalOID := lifecycleGit(t, repo, "rev-parse", "HEAD")
	lifecycleGit(t, repo, "commit", "--allow-empty", "-m", "remote head")
	lifecycleGit(t, repo, "push", bare, "HEAD:refs/heads/topic")
	lifecycleGit(t, repo, "remote", "add", "origin", bare)
	lifecycleGit(t, repo, "update-ref", "refs/kit/merge-requests/2/head", originalOID)
	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{
		ProjectRoot: repo,
	})
	require.NoError(t, err)
	require.NoError(t, backend.Validate(t.Context()))

	_, err = backend.FetchExpected(
		t.Context(), "origin", "refs/heads/topic", "refs/kit/merge-requests/2/head",
		strings.Repeat("b", 40),
	)

	var typed *ChangeRequestError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, ChangeRequestHeadChanged, typed.Kind)
	assert.Equal(t, originalOID,
		lifecycleGit(t, repo, "rev-parse", "refs/kit/merge-requests/2/head"),
		"a per-call OID mismatch must not publish over the destination ref")
}

func TestChangeRequestGitFetchRejectsSymbolicDestination(t *testing.T) {
	repo := initLifecycleRepo(t)
	bare := filepath.Join(t.TempDir(), "origin.git")
	lifecycleGit(t, repo, "init", "--bare", bare)
	lifecycleGit(t, repo, "commit", "--allow-empty", "-m", "remote head")
	lifecycleGit(t, repo, "push", bare, "HEAD:refs/heads/topic")
	lifecycleGit(t, repo, "remote", "add", "origin", bare)
	mainOID := lifecycleGit(t, repo, "rev-parse", "refs/heads/main")
	lifecycleGit(t, repo, "symbolic-ref", "refs/kit/merge-requests/3/head", "refs/heads/main")
	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{ProjectRoot: repo})
	require.NoError(t, err)
	require.NoError(t, backend.Validate(t.Context()))

	_, err = backend.Fetch(
		t.Context(), "origin", "refs/heads/topic", "refs/kit/merge-requests/3/head",
	)

	var typed *ChangeRequestError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, ChangeRequestUnsafeConfiguration, typed.Kind)
	assert.Equal(t, mainOID, lifecycleGit(t, repo, "rev-parse", "refs/heads/main"))
}

func TestChangeRequestGitFetchRejectsCaseInsensitiveDestinationAlias(t *testing.T) {
	repo := initLifecycleRepo(t)
	lifecycleGit(t, repo, "update-ref", "refs/kit/merge-requests/4/Head", "HEAD")
	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{ProjectRoot: repo})
	require.NoError(t, err)

	_, err = backend.Fetch(
		t.Context(), "origin", "refs/heads/topic", "refs/kit/merge-requests/4/head",
	)

	var typed *ChangeRequestError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, ChangeRequestUnsafeConfiguration, typed.Kind)
}

func TestChangeRequestGitFetchRejectsLeadingOptionArguments(t *testing.T) {
	repo := initLifecycleRepo(t)
	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{ProjectRoot: repo})
	require.NoError(t, err)

	for _, test := range []struct {
		name      string
		remote    string
		sourceRef string
	}{
		{name: "remote", remote: "--upload-pack=evil", sourceRef: "refs/heads/topic"},
		{name: "source ref", remote: "origin", sourceRef: "--upload-pack=evil"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, fetchErr := backend.Fetch(t.Context(), test.remote, test.sourceRef,
				"refs/kit/merge-requests/5/head")
			var typed *ChangeRequestError
			require.ErrorAs(t, fetchErr, &typed)
			assert.Equal(t, ChangeRequestUnsafeConfiguration, typed.Kind)
		})
	}
}

func TestChangeRequestGitFetchTerminatesOptions(t *testing.T) {
	repo := initLifecycleRepo(t)
	bare := filepath.Join(t.TempDir(), "origin.git")
	lifecycleGit(t, repo, "init", "--bare", bare)
	lifecycleGit(t, repo, "push", bare, "HEAD:refs/heads/topic")
	lifecycleGit(t, repo, "remote", "add", "origin", bare)
	var fetchArgs []string
	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{
		ProjectRoot: repo,
		RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "fetch" {
				fetchArgs = append([]string(nil), args...)
			}
			stdout, stderr, runErr := runner.Run(ctx, dir, nil, args...)
			return append(stdout, stderr...), runErr
		},
	})
	require.NoError(t, err)
	require.NoError(t, backend.Validate(t.Context()))

	_, err = backend.Fetch(t.Context(), "origin", "refs/heads/topic", "refs/kit/merge-requests/6/head")

	require.NoError(t, err)
	require.Len(t, fetchArgs, 5)
	assert.Equal(t, []string{"fetch", "--no-tags", "--", "origin"}, fetchArgs[:4])
	assert.True(t, strings.HasPrefix(fetchArgs[4], "+refs/heads/topic:refs/kit/tmp/fetch-"))
}

func TestChangeRequestGitFetchRejectsUnvalidatedRawRemote(t *testing.T) {
	repo := initLifecycleRepo(t)
	bare := filepath.Join(t.TempDir(), "origin.git")
	lifecycleGit(t, repo, "init", "--bare", bare)
	lifecycleGit(t, repo, "push", bare, "HEAD:refs/heads/topic")
	lifecycleGit(t, repo, "remote", "add", "origin", bare)
	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{ProjectRoot: repo})
	require.NoError(t, err)
	require.NoError(t, backend.Validate(t.Context()))

	_, err = backend.Fetch(t.Context(), bare, "refs/heads/topic",
		"refs/kit/merge-requests/7/head")

	var typed *ChangeRequestError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, ChangeRequestUnsafeConfiguration, typed.Kind)
}

func TestChangeRequestGitFetchRevalidatesConfiguredRemoteURL(t *testing.T) {
	repo := initLifecycleRepo(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	replacement := filepath.Join(t.TempDir(), "replacement.git")
	lifecycleGit(t, repo, "init", "--bare", origin)
	lifecycleGit(t, repo, "init", "--bare", replacement)
	lifecycleGit(t, repo, "push", origin, "HEAD:refs/heads/topic")
	lifecycleGit(t, repo, "push", replacement, "HEAD:refs/heads/topic")
	lifecycleGit(t, repo, "remote", "add", "origin", origin)
	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{ProjectRoot: repo})
	require.NoError(t, err)
	require.NoError(t, backend.Validate(t.Context()))
	lifecycleGit(t, repo, "remote", "set-url", "origin", replacement)

	_, err = backend.Fetch(t.Context(), "origin", "refs/heads/topic",
		"refs/kit/merge-requests/8/head")

	var typed *ChangeRequestError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, ChangeRequestUnsafeConfiguration, typed.Kind)
}

func TestChangeRequestGitFetchRejectsTrustedDestinationNamespace(t *testing.T) {
	repo := initLifecycleRepo(t)
	bare := filepath.Join(t.TempDir(), "origin.git")
	lifecycleGit(t, repo, "init", "--bare", bare)
	lifecycleGit(t, repo, "commit", "--allow-empty", "-m", "remote head")
	lifecycleGit(t, repo, "push", bare, "HEAD:refs/heads/topic")
	lifecycleGit(t, repo, "remote", "add", "origin", bare)
	mainOID := lifecycleGit(t, repo, "rev-parse", "refs/heads/main")
	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{ProjectRoot: repo})
	require.NoError(t, err)
	require.NoError(t, backend.Validate(t.Context()))

	_, err = backend.Fetch(t.Context(), "origin", "refs/heads/topic", "refs/heads/main")

	var typed *ChangeRequestError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, ChangeRequestUnsafeConfiguration, typed.Kind)
	assert.Equal(t, mainOID, lifecycleGit(t, repo, "rev-parse", "refs/heads/main"))
}

func TestChangeRequestGitFetchUsesPrivateTemporaryRef(t *testing.T) {
	repo := initLifecycleRepo(t)
	bare := filepath.Join(t.TempDir(), "origin.git")
	lifecycleGit(t, repo, "init", "--bare", bare)
	mainOID := lifecycleGit(t, repo, "rev-parse", "HEAD")
	lifecycleGit(t, repo, "commit", "--allow-empty", "-m", "remote head")
	topicOID := lifecycleGit(t, repo, "rev-parse", "HEAD")
	lifecycleGit(t, repo, "push", bare, "HEAD:refs/heads/topic")
	lifecycleGit(t, repo, "remote", "add", "origin", bare)
	raced := false
	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{
		ProjectRoot: repo,
		RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
			stdout, stderr, runErr := runner.Run(ctx, dir, nil, args...)
			if runErr == nil && !raced && len(args) > 0 && args[0] == "fetch" {
				raced = true
				require.NoError(t, os.WriteFile(filepath.Join(repo, ".git", "FETCH_HEAD"),
					[]byte(mainOID+"\t\tbranch 'main' of .\n"), 0o600))
			}
			return append(stdout, stderr...), runErr
		},
	})
	require.NoError(t, err)
	require.NoError(t, backend.Validate(t.Context()))

	_, err = backend.Fetch(t.Context(), "origin", "refs/heads/topic",
		"refs/kit/merge-requests/9/head")

	require.NoError(t, err)
	assert.True(t, raced)
	assert.NotEqual(t, mainOID, topicOID)
	assert.Equal(t, topicOID,
		lifecycleGit(t, repo, "rev-parse", "refs/kit/merge-requests/9/head"))
	assert.Empty(t, lifecycleGit(t, repo, "for-each-ref", "--format=%(refname)", "refs/kit/tmp"))
}

func TestChangeRequestGitCanonicalizesRelativeProjectCloneURL(t *testing.T) {
	project := initLifecycleRepo(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	lifecycleGit(t, project, "init", "--bare", origin)
	relative, err := filepath.Rel(project, origin)
	require.NoError(t, err)
	lifecycleGit(t, project, "remote", "add", "origin", relative)
	backend, err := NewChangeRequestGit(ChangeRequestGitOptions{
		ProjectRoot:     project,
		ProjectCloneURL: relative,
	})
	require.NoError(t, err)

	require.NoError(t, backend.Validate(t.Context()))
	assert.Equal(t, origin, backend.project.CloneURL)
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

func TestChangeRequestGitEnsureRemoteSerializesRepositoryMutation(t *testing.T) {
	repo := initLifecycleRepo(t)
	lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
	entered := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	runGit := func(
		ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
	) ([]byte, error) {
		if len(args) == 1 && args[0] == "remote" {
			entered <- struct{}{}
			<-release
		}
		stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
		return append(stdout, stderr...), err
	}
	newBackend := func() *ChangeRequestGit {
		backend, err := NewChangeRequestGit(ChangeRequestGitOptions{
			ProjectRoot: repo,
			ProjectIdentity: gitremote.Identity{
				Host: "github.com", Owner: "acme", Name: "widget",
			},
			RunGit: runGit,
		})
		require.NoError(t, err)
		return backend
	}
	firstBackend := newBackend()
	secondBackend := newBackend()

	results := make(chan error, 2)
	started := make(chan struct{}, 2)
	go func() {
		started <- struct{}{}
		_, err := firstBackend.EnsureRemote(t.Context(), changeRequestRemote("octocat"))
		results <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first remote mutation did not start")
	}
	go func() {
		started <- struct{}{}
		_, err := secondBackend.EnsureRemote(t.Context(), changeRequestRemote("octocat"))
		results <- err
	}()
	<-started
	<-started
	select {
	case <-entered:
		release <- struct{}{}
		release <- struct{}{}
		require.NoError(t, <-results)
		require.NoError(t, <-results)
		t.Fatal("second remote mutation entered before the first released its repository lock")
	case <-time.After(150 * time.Millisecond):
	}
	release <- struct{}{}
	require.NoError(t, <-results)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("second remote mutation did not resume")
	}
	release <- struct{}{}
	require.NoError(t, <-results)
}

func TestChangeRequestGitEnsureRemoteRejectsCloneURLIdentityMismatch(t *testing.T) {
	repo, backend := newChangeRequestGit(t)
	lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
	repository := changeRequestRemote("octocat")
	repository.CloneURL = "https://evil.example/octocat/widget.git"

	_, err := backend.EnsureRemote(t.Context(), repository)

	var typed *ChangeRequestError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, ChangeRequestUnsafeConfiguration, typed.Kind)
	assert.NotContains(t, strings.Fields(lifecycleGit(t, repo, "remote")), "review-octocat")
}

func TestChangeRequestGitEnsureRemoteCanonicalizesRelativeLocalPath(t *testing.T) {
	repo, backend := newChangeRequestGit(t)
	target := filepath.Join(filepath.Dir(repo), "forks", "team", "widget")
	require.NoError(t, os.MkdirAll(target, 0o755))
	relative, err := filepath.Rel(repo, target)
	require.NoError(t, err)

	repository := RemoteRepository{CloneURL: relative}
	remote, err := backend.EnsureRemote(t.Context(), repository)

	require.NoError(t, err)
	assert.Equal(t, target, lifecycleGit(t, repo, "remote", "get-url", remote))
	created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Branch:      "relative-local-push",
		Path:        filepath.Join(t.TempDir(), "worktree"),
		BaseRef:     "HEAD",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = created.Rollback(context.Background()) })
	require.NoError(t, backend.ConfigurePush(
		t.Context(), created, remote, repository, "feature",
	))
}

func TestChangeRequestGitExpectedURLDoesNotReplaceIdentityValidation(t *testing.T) {
	repo, backend := newChangeRequestGit(t)
	remoteURL := "https://evil.example/octocat/widget.git"
	lifecycleGit(t, repo, "remote", "add", "fork", remoteURL)
	backend.rememberExpectedRemoteURL("fork", false, remoteURL)

	err := backend.validateEffectiveRemote(
		t.Context(), "", "fork", changeRequestRemote("octocat"),
	)

	var typed *ChangeRequestError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, ChangeRequestUnsafeConfiguration, typed.Kind)
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

func TestChangeRequestGitValidateRejectsMismatchedProjectOrigin(t *testing.T) {
	repo, backend := newChangeRequestGit(t)
	lifecycleGit(t, repo, "remote", "add", "origin", "https://evil.example/acme/widget.git")

	err := backend.Validate(t.Context())

	var typed *ChangeRequestError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, ChangeRequestUnsafeConfiguration, typed.Kind)
}

func TestChangeRequestGitDoesNotInheritSSHFromUnrelatedPushRemote(t *testing.T) {
	repo, backend := newChangeRequestGit(t)
	lifecycleGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
	lifecycleGit(t, repo, "remote", "add", "backup", "ssh://git@evil.example/acme/widget.git")
	lifecycleGit(t, repo, "config", "remote.pushDefault", "backup")

	remote, err := backend.EnsureRemote(t.Context(), changeRequestRemote("octocat"))

	require.NoError(t, err)
	assert.Equal(t, "https://github.com/octocat/widget.git",
		lifecycleGit(t, repo, "remote", "get-url", remote))
}

func TestChangeRequestGitRejectsDerivedSSHURLForDifferentHost(t *testing.T) {
	repo, backend := newChangeRequestGit(t)
	lifecycleGit(t, repo, "remote", "add", "origin", "ssh://git@github.com/acme/widget.git")
	repository := RemoteRepository{
		Identity: gitremote.Identity{Host: "gitlab.example", Owner: "octocat", Name: "widget"},
		CloneURL: "https://gitlab.example/octocat/widget.git",
	}

	_, err := backend.EnsureRemote(t.Context(), repository)

	var typed *ChangeRequestError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, ChangeRequestUnsafeConfiguration, typed.Kind)
	assert.NotContains(t, strings.Fields(lifecycleGit(t, repo, "remote")), "review-octocat")
}

func TestRemoteMatchesRepositoryRequiresHostForHostedIdentity(t *testing.T) {
	repository := changeRequestRemote("octocat")

	assert.False(t, remoteMatchesRepository(
		filepath.Join(t.TempDir(), "widget.git"), repository,
	))
	assert.False(t, remoteMatchesRepository(
		"file:///tmp/widget.git", repository,
	))
	assert.False(t, remoteMatchesRepository(
		"ssh://git@github.com", repository,
	))
	assert.True(t, remoteMatchesRepository(
		"ssh://git@github.com/octocat/widget.git", repository,
	))
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
	require.NoError(t, safefileio.ValidatePrivateDir(hooksPath))
}

func TestChangeRequestGitConfigurePushAcceptsDistinctValidatedRemoteURLs(t *testing.T) {
	require := require.New(t)
	repo, backend := newChangeRequestGit(t)
	repository := changeRequestRemote("octocat")
	lifecycleGit(t, repo, "remote", "add", "fork", repository.CloneURL)
	pushURL := "git@github.com:octocat/widget.git"
	lifecycleGit(t, repo, "remote", "set-url", "--push", "fork", pushURL)
	remote, err := backend.EnsureRemote(t.Context(), repository)
	require.NoError(err)
	require.Equal("fork", remote)
	created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Path:        filepath.Join(t.TempDir(), "mixed-transport-worktree"),
		Branch:      "review-mixed-transport",
		BaseRef:     "main",
	})
	require.NoError(err)
	t.Cleanup(func() { _, _ = created.Rollback(context.Background()) })

	err = backend.ConfigurePush(t.Context(), created, remote, repository, "feature/widgets")

	require.NoError(err)
	require.Equal(repository.CloneURL, lifecycleGit(t, repo, "remote", "get-url", remote))
	require.Equal(pushURL, lifecycleGit(t, repo, "remote", "get-url", "--push", remote))
}

func TestChangeRequestGitConfigurePushAcceptsDistinctValidatedProjectURLs(t *testing.T) {
	require := require.New(t)
	repo, backend := newChangeRequestGit(t)
	repository := changeRequestRemote("acme")
	lifecycleGit(t, repo, "remote", "add", "origin", repository.CloneURL)
	pushURL := "git@github.com:acme/widget.git"
	lifecycleGit(t, repo, "remote", "set-url", "--push", "origin", pushURL)
	require.NoError(backend.Validate(t.Context()))
	created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Path:        filepath.Join(t.TempDir(), "mixed-project-transport-worktree"),
		Branch:      "review-mixed-project-transport",
		BaseRef:     "main",
	})
	require.NoError(err)
	t.Cleanup(func() { _, _ = created.Rollback(context.Background()) })

	err = backend.ConfigurePush(t.Context(), created, "origin", repository, "feature/widgets")

	require.NoError(err)
	require.Equal(repository.CloneURL, lifecycleGit(t, repo, "remote", "get-url", "origin"))
	require.Equal(pushURL, lifecycleGit(t, repo, "remote", "get-url", "--push", "origin"))
}

func TestChangeRequestGitConfigurePushRejectsChangedValidatedPushURL(t *testing.T) {
	repo, backend := newChangeRequestGit(t)
	repository := changeRequestRemote("octocat")
	lifecycleGit(t, repo, "remote", "add", "fork", repository.CloneURL)
	lifecycleGit(t, repo, "remote", "set-url", "--push", "fork",
		"git@github.com:octocat/widget.git")
	remote, err := backend.EnsureRemote(t.Context(), repository)
	require.NoError(t, err)
	lifecycleGit(t, repo, "remote", "set-url", "--push", "fork",
		"ssh://git@github.com/octocat/widget.git")
	created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: repo,
		Path:        filepath.Join(t.TempDir(), "changed-push-worktree"),
		Branch:      "review-changed-push",
		BaseRef:     "main",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = created.Rollback(context.Background()) })

	err = backend.ConfigurePush(t.Context(), created, remote, repository, "feature/widgets")

	var typed *ChangeRequestError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, ChangeRequestUnsafeConfiguration, typed.Kind)
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

	remote, err := backend.EnsureRemote(t.Context(), RemoteRepository{CloneURL: bare})
	require.NoError(t, err)
	_, err = backend.Fetch(context.Background(), remote, "refs/heads/topic",
		"refs/kit/merge-requests/10/head")

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

func TestSSHRemoteURLRejectsLocalPaths(t *testing.T) {
	tests := []struct {
		name      string
		remoteURL string
		want      bool
	}{
		{name: "scp", remoteURL: "git@example.com:owner/repo.git", want: true},
		{name: "ssh URL", remoteURL: "ssh://git@example.com/owner/repo.git", want: true},
		{name: "ssh plus git URL", remoteURL: "ssh+git://git@example.com/owner/repo.git", want: true},
		{name: "Windows drive", remoteURL: `D:\work\repo.git`},
		{name: "Windows slash drive", remoteURL: "D:/work/repo.git"},
		{name: "Unix absolute", remoteURL: "/work/repo.git"},
		{name: "file URL", remoteURL: "file:///work/repo.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isSSHRemoteURL(tt.remoteURL))
		})
	}
}
