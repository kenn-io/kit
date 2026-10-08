package managedworktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func TestConfiguredPushUsesSelectedUpstream(t *testing.T) {
	for _, scope := range []UpstreamScope{UpstreamWorktree, UpstreamRepository} {
		t.Run(map[UpstreamScope]string{UpstreamWorktree: "worktree", UpstreamRepository: "repository"}[scope], func(t *testing.T) {
			_, clone := initOriginAndClone(t)
			created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: lifecycleTestRunner(t),
			})
			require.NoError(t, err)
			lifecycleGit(t, clone, "config", "extensions.worktreeConfig", "true")
			lifecycleGit(t, clone, "config", "--global", "push.default", "simple")
			lifecycleGit(t, clone, "config", "--local", "push.default", "current")
			lifecycleGit(t, created.Path, "config", "--worktree", "push.default", "current")
			err = SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
				ProjectRoot: clone, Path: created.Path, Runner: lifecycleTestRunner(t),
				Policy: UpstreamPolicy{Action: UpstreamTrack, Scope: scope, Remote: "origin", Ref: "refs/heads/main", ConfigurePush: true},
			})
			require.NoError(t, err)
			push := lifecycleGit(t, created.Path, "push", "--dry-run", "--porcelain")
			assert.Contains(t, push, "refs/heads/topic:refs/heads/main")
			assert.NotContains(t, push, "refs/heads/topic:refs/heads/topic")
		})
	}
}

func TestConfiguredPushRejectsCommandOverrideBeforeChangingRouting(t *testing.T) {
	for _, source := range []string{"runner", "environment"} {
		t.Run(source, func(t *testing.T) {
			_, clone := initOriginAndClone(t)
			created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: lifecycleTestRunner(t),
			})
			require.NoError(t, err)
			runner := lifecycleTestRunner(t)
			if source == "runner" {
				runner = runner.WithConfig("push.default", "current")
			} else {
				runner.Env = append(runner.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=push.default", "GIT_CONFIG_VALUE_0=current")
			}
			before, err := os.ReadFile(filepath.Join(clone, ".git", "config"))
			require.NoError(t, err)
			err = SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
				ProjectRoot: clone, Path: created.Path, Runner: runner,
				Policy: UpstreamPolicy{Action: UpstreamTrack, Scope: UpstreamRepository, Remote: "origin", Ref: "refs/heads/main", ConfigurePush: true},
			})
			require.ErrorIs(t, err, ErrInvalidWorktreeOptions)
			after, err := os.ReadFile(filepath.Join(clone, ".git", "config"))
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestLifecycleExecutionOptionsPreserveInheritedConfig(t *testing.T) {
	if os.Getenv("KIT_TEST_INHERITED_CONFIG") != "1" {
		// A nil runner environment must inherit the process environment. Use a
		// child with fixture-only Git settings so host bindings cannot escape.
		isolateLifecycleGitConfig(t)
		executable, err := os.Executable()
		require.NoError(t, err)
		cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestLifecycleExecutionOptionsPreserveInheritedConfig$")
		cmd.Env = append(lifecycleGitEnv(t), "KIT_TEST_INHERITED_CONFIG=1",
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=fixture.value", "GIT_CONFIG_VALUE_0=inherited")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return
	}
	for _, tc := range []struct {
		name   string
		runner gitcmd.Runner
	}{
		{"wait delay", gitcmd.Runner{WaitDelay: time.Second}},
		{"stdout limit", gitcmd.Runner{StdoutLimit: 1 << 20}},
		{"stderr limit", gitcmd.Runner{StderrLimit: 1 << 20}},
		{"accept wait delay", gitcmd.Runner{AcceptSuccessfulWaitDelay: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initLifecycleRepo(t)
			observed := false
			_, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: tc.runner,
				RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
					if !observed {
						out, err := runner.Output(ctx, dir, "config", "--get", "fixture.value")
						require.NoError(t, err)
						assert.Equal(t, "inherited\n", string(out))
						observed = true
					}
					out, stderr, err := runner.Run(ctx, dir, nil, args...)
					return append(out, stderr...), err
				},
			})
			require.NoError(t, err)
			assert.True(t, observed)
		})
	}
}

func TestCreateExplicitCheckoutModes(t *testing.T) {
	for _, bare := range []bool{false, true} {
		t.Run(map[bool]string{false: "checkout", true: "bare"}[bare], func(t *testing.T) {
			root := initLifecycleRepo(t)
			require.NoError(t, os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("original\n"), 0o600))
			lifecycleGit(t, root, "add", "tracked.txt")
			lifecycleGit(t, root, "commit", "-m", "tracked content")
			if bare {
				clone := filepath.Join(t.TempDir(), "bare.git")
				lifecycleGit(t, root, "clone", "--bare", root, clone)
				root = clone
			}
			runner := lifecycleTestRunner(t)
			lifecycleGit(t, root, "branch", "existing")
			attached, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Branch: "existing", Path: filepath.Join(t.TempDir(), "attached"),
				Mode: CheckoutExistingBranch, Runner: runner,
			})
			require.NoError(t, err)
			assert.False(t, attached.BranchCreated)
			assert.Equal(t, "existing", lifecycleGit(t, attached.Path, "branch", "--show-current"))
			_, err = CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Branch: "missing", Path: filepath.Join(t.TempDir(), "missing"),
				Mode: CheckoutExistingBranch, Runner: runner,
			})
			require.ErrorIs(t, err, ErrBranchNotFound)
			assert.False(t, branchExistsInRepo(t, root, "missing"))
			_, err = CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Branch: "existing", Path: filepath.Join(t.TempDir(), "collision"),
				Mode: CheckoutNewBranch, Runner: runner,
			})
			require.ErrorIs(t, err, ErrBranchAlreadyExists)
			detached, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Path: filepath.Join(t.TempDir(), "detached"), BaseRef: "HEAD",
				Mode: CheckoutDetached, NoCheckout: true, LockReason: "fixture-preparing", Runner: runner,
			})
			require.NoError(t, err)
			assert.Empty(t, detached.Branch)
			assert.False(t, detached.BranchCreated)
			assert.Empty(t, lifecycleGit(t, detached.Path, "branch", "--show-current"))
			assert.NoFileExists(t, filepath.Join(detached.Path, "tracked.txt"))
			registration := lifecycleGit(t, detached.Path, "rev-parse", "--absolute-git-dir")
			marker, err := os.ReadFile(filepath.Join(registration, "locked"))
			require.NoError(t, err)
			assert.Equal(t, "fixture-preparing", strings.TrimSpace(string(marker)))
		})
	}
}

func TestCreateDeferredFailureReportsAcquisition(t *testing.T) {
	for _, phase := range []string{"add", "snapshot", "checkout", "tracking", "hook"} {
		t.Run(phase, func(t *testing.T) {
			root := initLifecycleRepo(t)
			lifecycleGit(t, root, "remote", "add", "selected", root)
			path := filepath.Join(t.TempDir(), "checkout")
			failure := errors.New("fixture operation failed")
			failed := false
			hook := writeHookScript(t, root, filepath.Join(t.TempDir(), "hook-output"), 0)
			result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Branch: "topic", Path: path, Runner: lifecycleTestRunner(t),
				Mode: CheckoutNewBranch, Checkout: CheckoutIsolated, FailureCleanup: CleanupDeferred, SetupScript: hook,
				Upstream: UpstreamPolicy{Action: UpstreamTrack, Remote: "selected", Ref: "refs/heads/main"},
				RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
					out, stderr, err := runner.Run(ctx, dir, nil, args...)
					if !failed && ((phase == "add" && len(args) > 1 && args[0] == "worktree" && args[1] == "add") ||
						(phase == "snapshot" && dir == path && slices.Contains(args, "--absolute-git-dir")) ||
						(phase == "checkout" && slices.Contains(args, "reset")) ||
						(phase == "tracking" && slices.Contains(args, "--replace-all") && slices.Contains(args, "branch.topic.remote"))) {
						failed = true
						return append(out, stderr...), errors.Join(err, failure)
					}
					return append(out, stderr...), err
				},
				RunHook: func(ctx context.Context, command HookCommand) error {
					if phase == "hook" {
						return failure
					}
					return runTestHook(ctx, command)
				},
			})
			require.ErrorIs(t, err, failure)
			assert.Equal(t, path, result.Path)
			assert.Equal(t, "topic", result.Branch)
			assert.DirExists(t, path)
			assert.True(t, branchExistsInRepo(t, root, "topic"))
		})
	}
}

func TestPreparedHookRetainsValidationAndEnvironment(t *testing.T) {
	root := initLifecycleRepo(t)
	path := filepath.Join(t.TempDir(), "worktree")
	output := filepath.Join(t.TempDir(), "hook-output")
	script := writeHookScript(t, root, output, 0)
	prepared, err := PrepareWorktreeHook(t.Context(), WorktreeHookOptions{
		ProjectRoot: root, Path: path, Branch: "topic", Script: script,
		WorktreeName: "display-name", RunHook: testHookRunner(),
	})
	require.NoError(t, err)
	assert.NoFileExists(t, output)
	_, err = CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: root, Path: path, Branch: "topic", Runner: lifecycleTestRunner(t),
	})
	require.NoError(t, err)
	require.NoError(t, prepared.Run(t.Context()))
	contents, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Contains(t, string(contents), "name=display-name")
	assert.Contains(t, string(contents), "branch=topic")
	_, err = PrepareWorktreeHook(t.Context(), WorktreeHookOptions{
		ProjectRoot: root, Path: path, Script: filepath.Join(filepath.Dir(root), "outside"),
	})
	require.ErrorIs(t, err, ErrHookOutsideProject)
	_, err = PrepareWorktreeHook(t.Context(), WorktreeHookOptions{
		ProjectRoot: root, Path: path, Script: filepath.Join(path, "hook"), MergeRequest: true,
	})
	require.Error(t, err)
}

func TestImportExplicitUpstreamPolicy(t *testing.T) {
	for _, action := range []UpstreamAction{UpstreamLeave, UpstreamClear, UpstreamTrack} {
		t.Run(map[UpstreamAction]string{UpstreamLeave: "leave", UpstreamClear: "clear", UpstreamTrack: "track"}[action], func(t *testing.T) {
			origin, clone := initOriginAndClone(t)
			lifecycleGit(t, clone, "remote", "add", "selected", origin)
			lifecycleGit(t, clone, "fetch", "selected")
			lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "new import head")
			lifecycleGit(t, clone, "config", "branch.topic.remote", "kept")
			lifecycleGit(t, clone, "config", "branch.topic.merge", "refs/heads/kept")
			result, err := CreateWorktreeFromMergeRequest(t.Context(), MergeRequestWorktreeOptions{
				ProjectRoot: clone, Branch: "topic", Path: filepath.Join(t.TempDir(), "checkout"),
				Number: 7, HeadBranch: "main", HeadRepoCloneURL: origin, ProjectRepoIdentity: origin,
				Runner: lifecycleTestRunner(t), FailureCleanup: CleanupDeferred,
				Upstream: UpstreamPolicy{Action: action, Remote: "selected", Ref: "refs/heads/main"},
			})
			require.NoError(t, err)
			assert.NotEqual(t, lifecycleGit(t, clone, "rev-parse", "selected/main"), lifecycleGit(t, result.Path, "rev-parse", "HEAD"))
			switch action {
			case UpstreamDefault:
				require.FailNow(t, "test requires an explicit upstream action")
			case UpstreamLeave:
				assert.Equal(t, "kept", worktreeConfig(t, result.Path, "branch.topic.remote"))
				assert.Equal(t, "refs/heads/kept", worktreeConfig(t, result.Path, "branch.topic.merge"))
			case UpstreamClear:
				assert.Empty(t, worktreeConfig(t, result.Path, "branch.topic.remote"))
				assert.Empty(t, worktreeConfig(t, result.Path, "branch.topic.merge"))
			case UpstreamTrack:
				assert.Equal(t, "selected", worktreeOnlyConfig(t, result.Path, "branch.topic.remote"))
				assert.Equal(t, "refs/heads/main", worktreeOnlyConfig(t, result.Path, "branch.topic.merge"))
			}
		})
	}
}

func TestExplicitUpstreamScopeAndHeadCondition(t *testing.T) {
	for _, condition := range []TrackingCondition{TrackingExplicit, TrackingIfHeadMatches} {
		t.Run(map[TrackingCondition]string{TrackingExplicit: "explicit", TrackingIfHeadMatches: "matching"}[condition], func(t *testing.T) {
			origin, clone := initOriginAndClone(t)
			lifecycleGit(t, clone, "remote", "add", "selected", origin)
			lifecycleGit(t, clone, "config", "remote.selected.fetch", "+refs/heads/*:refs/tracked/selected/*")
			lifecycleGit(t, clone, "fetch", "selected")
			lifecycleGit(t, clone, "commit", "--allow-empty", "-m", "local advance")
			result, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: clone, Branch: "topic", Path: filepath.Join(t.TempDir(), "checkout"),
				Runner:   lifecycleTestRunner(t),
				Upstream: UpstreamPolicy{Action: UpstreamTrack, Scope: UpstreamRepository, Condition: condition, Remote: "selected", Ref: "refs/heads/main"},
			})
			require.NoError(t, err)
			if condition == TrackingIfHeadMatches {
				assert.Empty(t, worktreeConfig(t, result.Path, "branch.topic.remote"))
				lifecycleGit(t, result.Path, "reset", "--hard", "refs/tracked/selected/main")
				err = SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
					ProjectRoot: clone, Path: result.Path, Runner: lifecycleTestRunner(t),
					Policy: UpstreamPolicy{Action: UpstreamTrack, Scope: UpstreamRepository, Condition: condition, Remote: "selected", Ref: "refs/heads/main"},
				})
				require.NoError(t, err)
			}
			assert.Equal(t, "selected", lifecycleGit(t, clone, "config", "--local", "branch.topic.remote"))
			assert.Equal(t, "refs/tracked/selected/main", lifecycleGit(t, result.Path, "rev-parse", "--symbolic-full-name", "@{upstream}"))
			assert.Empty(t, worktreeConfig(t, result.Path, "branch.topic.pushRemote"))
			err = SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
				ProjectRoot: clone, Path: result.Path, Runner: lifecycleTestRunner(t),
				Policy: UpstreamPolicy{Action: UpstreamClear, Scope: UpstreamRepository},
			})
			require.NoError(t, err)
			_, _, err = lifecycleTestRunner(t).Run(t.Context(), clone, nil, "config", "--local", "--get", "branch.topic.remote")
			require.True(t, gitcmd.IsExitCode(err, 1))
		})
	}
}

func TestImportExplicitCheckoutModes(t *testing.T) {
	for _, mode := range []CheckoutMode{CheckoutExistingBranch, CheckoutDetached} {
		t.Run(map[CheckoutMode]string{CheckoutExistingBranch: "attach", CheckoutDetached: "detached"}[mode], func(t *testing.T) {
			origin, clone := initOriginAndClone(t)
			require.NoError(t, os.WriteFile(filepath.Join(origin, "tracked"), []byte("content"), 0o600))
			lifecycleGit(t, origin, "add", "tracked")
			lifecycleGit(t, origin, "commit", "-m", "tracked file")
			lifecycleGit(t, clone, "fetch", "origin")
			branch := ""
			if mode == CheckoutExistingBranch {
				branch = "existing"
				lifecycleGit(t, clone, "branch", branch, "origin/main")
			}
			result, err := CreateWorktreeFromMergeRequest(t.Context(), MergeRequestWorktreeOptions{
				ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: branch,
				Mode: mode, NoCheckout: true, Number: 1, Runner: lifecycleTestRunner(t),
				HeadBranch: "main", HeadRepoCloneURL: origin, ProjectRepoIdentity: origin,
			})
			require.NoError(t, err)
			assert.False(t, result.BranchCreated)
			assert.Equal(t, branch, lifecycleGit(t, result.Path, "branch", "--show-current"))
			assert.NoFileExists(t, filepath.Join(result.Path, "tracked"))
			assert.Equal(t, lifecycleGit(t, origin, "rev-parse", "HEAD"), lifecycleGit(t, result.Path, "rev-parse", "HEAD"))
		})
	}
}

func TestUpstreamActionsReplaceInheritedRouting(t *testing.T) {
	for _, scope := range []UpstreamScope{UpstreamWorktree, UpstreamRepository} {
		t.Run(map[UpstreamScope]string{UpstreamWorktree: "worktree", UpstreamRepository: "repository"}[scope], func(t *testing.T) {
			origin, clone := initOriginAndClone(t)
			lifecycleGit(t, origin, "checkout", "-b", "other")
			lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "other work")
			lifecycleGit(t, clone, "fetch", "origin")
			lifecycleGit(t, clone, "config", "branch.topic.remote", "origin")
			lifecycleGit(t, clone, "config", "branch.topic.merge", "refs/heads/main")
			created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", BaseRef: "origin/other",
				Runner:   lifecycleTestRunner(t),
				Upstream: UpstreamPolicy{Action: UpstreamTrack, Scope: scope, Remote: "origin", Ref: "refs/heads/other"},
			})
			require.NoError(t, err)
			assert.Equal(t, "refs/remotes/origin/other", lifecycleGit(t, created.Path, "rev-parse", "--symbolic-full-name", "@{upstream}"))
			lifecycleGit(t, created.Path, "pull", "--ff-only")
			require.NoError(t, SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
				ProjectRoot: clone, Path: created.Path, Runner: lifecycleTestRunner(t),
				Policy: UpstreamPolicy{Action: UpstreamClear, Scope: scope},
			}))
			_, _, err = lifecycleTestRunner(t).Run(t.Context(), created.Path, nil, "rev-parse", "--verify", "@{upstream}")
			require.True(t, gitcmd.IsExitCode(err, 128), "clearing tracking must produce an ordinary no-upstream error: %v", err)
			assert.Equal(t, "refs/heads/main", worktreeConfig(t, clone, "branch.main.merge"))
		})
	}
}

func TestConditionalTrackingUsesRequestedTarget(t *testing.T) {
	for _, matches := range []bool{false, true} {
		t.Run(map[bool]string{false: "mismatch", true: "match"}[matches], func(t *testing.T) {
			origin, clone := initOriginAndClone(t)
			lifecycleGit(t, origin, "checkout", "-b", "other")
			lifecycleGit(t, origin, "commit", "--allow-empty", "-m", "other work")
			lifecycleGit(t, clone, "fetch", "origin")
			start := "origin/main"
			if matches {
				start = "origin/other"
			}
			created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", BaseRef: start,
				Runner:   lifecycleTestRunner(t),
				Upstream: UpstreamPolicy{Action: UpstreamTrack, Scope: UpstreamRepository, Remote: "origin", Ref: "refs/heads/main"},
			})
			require.NoError(t, err)
			require.NoError(t, SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
				ProjectRoot: clone, Path: created.Path, Runner: lifecycleTestRunner(t),
				Policy: UpstreamPolicy{Action: UpstreamTrack, Condition: TrackingIfHeadMatches, Scope: UpstreamRepository, Remote: "origin", Ref: "refs/heads/other"},
			}))
			want := "refs/remotes/origin/main"
			if matches {
				want = "refs/remotes/origin/other"
			}
			assert.Equal(t, want, lifecycleGit(t, created.Path, "rev-parse", "--symbolic-full-name", "@{upstream}"))
		})
	}
}

func TestWorktreeUpstreamClearRemovesInheritedRouting(t *testing.T) {
	origin, clone := initOriginAndClone(t)
	created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
		ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: lifecycleTestRunner(t),
		Upstream: UpstreamPolicy{Action: UpstreamTrack, Scope: UpstreamRepository, Remote: "origin", Ref: "refs/heads/main"},
	})
	require.NoError(t, err)
	require.NoError(t, SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
		ProjectRoot: clone, Path: created.Path, Runner: lifecycleTestRunner(t),
		Policy: UpstreamPolicy{Action: UpstreamClear},
	}))
	_, _, err = lifecycleTestRunner(t).Run(t.Context(), created.Path, nil, "rev-parse", "--verify", "@{upstream}")
	require.True(t, gitcmd.IsExitCode(err, 128), "expected an ordinary no-upstream error: %v", err)
	assert.Equal(t, origin, lifecycleGit(t, clone, "remote", "get-url", "origin"))
}

func TestUpstreamRefusesRoutingFromOtherConfigFiles(t *testing.T) {
	for _, source := range []string{"global", "include", "command"} {
		for _, action := range []UpstreamAction{UpstreamTrack, UpstreamClear} {
			t.Run(source+"/"+map[UpstreamAction]string{UpstreamTrack: "track", UpstreamClear: "clear"}[action], func(t *testing.T) {
				_, clone := initOriginAndClone(t)
				external := filepath.Join(t.TempDir(), "external.gitconfig")
				contents := []byte("[branch \"topic\"]\nremote = origin\nmerge = refs/heads/main\n")
				require.NoError(t, os.WriteFile(external, contents, 0o600))
				runner := lifecycleTestRunner(t)
				switch source {
				case "global":
					runner.Env = append(runner.Env, "GIT_CONFIG_GLOBAL="+external)
				case "include":
					lifecycleGit(t, clone, "config", "include.path", external)
				case "command":
					runner = runner.WithConfig("branch.topic.remote", "origin").WithConfig("branch.topic.merge", "refs/heads/main")
				}
				created, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
					ProjectRoot: clone, Path: filepath.Join(t.TempDir(), "checkout"), Branch: "topic", Runner: runner,
					Upstream: UpstreamPolicy{Action: UpstreamLeave},
				})
				require.NoError(t, err)
				before, err := os.ReadFile(filepath.Join(clone, ".git", "config"))
				require.NoError(t, err)
				err = SetWorktreeUpstream(t.Context(), WorktreeUpstreamOptions{
					ProjectRoot: clone, Path: created.Path, Runner: runner,
					Policy: UpstreamPolicy{Action: action, Remote: "origin", Ref: "refs/heads/main"},
				})
				require.ErrorIs(t, err, ErrInvalidWorktreeOptions)
				after, err := os.ReadFile(filepath.Join(clone, ".git", "config"))
				require.NoError(t, err)
				assert.Equal(t, before, after)
				after, err = os.ReadFile(external)
				require.NoError(t, err)
				assert.Equal(t, contents, after)
			})
		}
	}
}
