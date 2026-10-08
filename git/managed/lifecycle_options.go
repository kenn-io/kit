package managedworktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
)

// CheckoutMode chooses whether creation may acquire a new branch.
type CheckoutMode uint8

const (
	CheckoutAuto CheckoutMode = iota // Attach an existing branch or create a missing one.
	CheckoutNewBranch
	CheckoutExistingBranch
	CheckoutDetached
)

// CheckoutPolicy chooses whether the checked-out tree may select Git programs.
type CheckoutPolicy uint8

const (
	CheckoutTrusted CheckoutPolicy = iota
	CheckoutIsolated
)

// FailureCleanup controls cleanup after an unsuccessful creation.
type FailureCleanup uint8

const (
	CleanupAutomatic FailureCleanup = iota
	CleanupDeferred
)

// UpstreamAction chooses the worktree's branch routing.
type UpstreamAction uint8

const (
	UpstreamDefault UpstreamAction = iota
	UpstreamTrack
	UpstreamLeave
	UpstreamClear
)

// TrackingCondition optionally requires the selected upstream tip to match HEAD.
type TrackingCondition uint8

const (
	TrackingExplicit TrackingCondition = iota
	TrackingIfHeadMatches
)

// UpstreamScope selects the Git configuration file used for branch routing.
type UpstreamScope uint8

const (
	UpstreamWorktree UpstreamScope = iota
	UpstreamRepository
)

// UpstreamPolicy selects tracking before any tracking mutation occurs.
// Ref is a full refs/heads/ name on Remote. Leave never changes tracking config.
// Default preserves the entry point's ordinary tracking behavior.
type UpstreamPolicy struct {
	Action    UpstreamAction
	Condition TrackingCondition
	Scope     UpstreamScope
	Remote    string
	Ref       string
	// ConfigurePush also selects branch.pushRemote and push.default=upstream.
	// Leave and an unmatched head never change either fetch or push routing.
	ConfigurePush bool
}

var (
	ErrBranchNotFound         = errors.New("local branch not found")
	ErrInvalidWorktreeOptions = errors.New("invalid worktree options")
)

// WorktreeUpstreamOptions selects routing for a linked worktree in ProjectRoot.
type WorktreeUpstreamOptions struct {
	ProjectRoot string
	Path        string
	Policy      UpstreamPolicy
	Runner      gitcmd.Runner
	RunGit      GitRunner
}

// SetWorktreeUpstream updates branch routing in the selected configuration scope.
func SetWorktreeUpstream(ctx context.Context, opts WorktreeUpstreamOptions) error {
	execution := newLifecycleExecution(opts.Runner, opts.RunGit, nil)
	root, err := absRequired(opts.ProjectRoot, "project root")
	if err != nil {
		return err
	}
	path, err := absRequired(opts.Path, "worktree path")
	if err != nil {
		return err
	}
	return execution.setUpstream(ctx, root, path, opts.Policy)
}

func validateUpstreamPolicy(policy UpstreamPolicy) error {
	if policy.Action > UpstreamClear || policy.Condition > TrackingIfHeadMatches || policy.Scope > UpstreamRepository {
		return ErrInvalidWorktreeOptions
	}
	if policy.Action == UpstreamTrack && (strings.TrimSpace(policy.Remote) == "" ||
		!strings.HasPrefix(policy.Ref, "refs/heads/") || strings.TrimPrefix(policy.Ref, "refs/heads/") == "") {
		return fmt.Errorf("%w: tracking requires a remote and full branch ref", ErrInvalidWorktreeOptions)
	}
	return nil
}

func (e lifecycleExecution) setUpstream(ctx context.Context, root, path string, policy UpstreamPolicy) error {
	if err := validateUpstreamPolicy(policy); err != nil {
		return err
	}
	if policy.Action == UpstreamDefault || policy.Action == UpstreamLeave {
		return nil
	}
	rootCommon, err := e.run(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	pathCommon, err := e.run(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if comparableWorktreePath(strings.TrimSpace(string(rootCommon))) != comparableWorktreePath(strings.TrimSpace(string(pathCommon))) {
		return fmt.Errorf("%w: worktree belongs to another repository", ErrInvalidWorktreeOptions)
	}
	branchOut, err := e.run(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if gitcmd.IsExitCode(err, 1) && policy.Action == UpstreamClear {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve tracking branch: %w", err)
	}
	branch := strings.TrimSpace(string(branchOut))
	remote, ref := policy.Remote, policy.Ref
	if policy.Action == UpstreamTrack {
		if _, err := e.run(ctx, root, "check-ref-format", ref); err != nil {
			return err
		}
		if _, err := e.run(ctx, root, "remote", "get-url", "--", remote); err != nil {
			return err
		}
		if policy.Condition == TrackingIfHeadMatches {
			// Ask Git to map the remote ref through its configured fetch refspecs
			// without first writing branch configuration.
			probe := e.runner.WithConfig("branch."+branch+".remote", remote).
				WithConfig("branch."+branch+".merge", ref)
			trackingRef, err := e.runWithRunner(ctx, probe, path, "for-each-ref", "--format=%(upstream)", "refs/heads/"+branch)
			if err != nil {
				return err
			}
			trackingOID, err := e.run(ctx, root, "rev-parse", "--verify", strings.TrimSpace(string(trackingRef))+"^{commit}")
			if err != nil {
				return err
			}
			headOID, err := e.run(ctx, path, "rev-parse", "--verify", "HEAD^{commit}")
			if err != nil {
				return err
			}
			if strings.TrimSpace(string(trackingOID)) != strings.TrimSpace(string(headOID)) {
				return nil
			}
		}
	} else {
		remote, ref = "", ""
	}
	scope := "--local"
	if policy.Scope == UpstreamWorktree {
		// Shared core.bare/core.worktree require a caller-directed migration;
		// never change their meaning merely to configure tracking.
		checkCtx := withLifecycleExecution(ctx, e.runner, e.runGit, e.runHook)
		if err := validateWorktreeConfigCompatibility(checkCtx, root); err != nil {
			return err
		}
		if _, err := e.run(ctx, root, "config", "extensions.worktreeConfig", "true"); err != nil {
			return err
		}
		scope = "--worktree"
	}
	entries := []gitcmd.Config{
		{Key: "branch." + branch + ".remote", Value: remote},
		{Key: "branch." + branch + ".merge", Value: ref},
	}
	if policy.ConfigurePush {
		entries = append(entries, gitcmd.Config{Key: "branch." + branch + ".pushRemote", Value: remote})
	}
	for _, entry := range entries {
		if policy.Action == UpstreamClear {
			_, err := e.run(ctx, path, "config", scope, "--unset-all", entry.Key)
			if err != nil && !gitcmd.IsExitCode(err, 5) {
				return err
			}
			if policy.Scope == UpstreamRepository {
				continue
			}
			// An empty worktree value masks inherited routing without changing
			// another checkout's repository configuration.
		}
		if _, err := e.run(ctx, path, "config", scope, "--replace-all", entry.Key, entry.Value); err != nil {
			return err
		}
	}
	if policy.Action == UpstreamTrack && policy.ConfigurePush {
		_, err = e.run(ctx, path, "config", scope, "push.default", "upstream")
	}
	return err
}

func (e lifecycleExecution) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return e.runWithRunner(ctx, e.runner, dir, args...)
}

func (e lifecycleExecution) runWithRunner(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
	if e.runGit != nil {
		out, err := e.runGit(ctx, runner, dir, args...)
		if err != nil && ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		return out, err
	}
	stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
	return append(stdout, stderr...), err
}

func newLifecycleExecution(runner gitcmd.Runner, runGit GitRunner, runHook HookRunner) lifecycleExecution {
	if runner.Env == nil {
		isZero := len(runner.Config) == 0 && !runner.StripEnv && !runner.TerminalPrompt &&
			!runner.NullGlobalConfig && !runner.NoSystemConfig && !runner.DisableSafeDirectoryForward
		runner.Env = os.Environ()
		if isZero {
			runner.StripEnv = true
		}
	}
	return lifecycleExecution{runner: runner, runGit: runGit, runHook: runHook}
}

func creationResult(ctx context.Context, root, path, branch string, created bool) CreateWorktreeResult {
	return CreateWorktreeResult{
		Path: path, Branch: branch, BranchCreated: created,
		projectRoot: root, ownedPath: path, ownedBranch: branch, ownedBranchCreated: created,
		runner: lifecycleRunner(ctx), runGit: lifecycleGitRunner(ctx), runHook: lifecycleHookRunner(ctx),
	}
}

// failedWorktreeAdd reports artifacts observed after an unsuccessful add.
// They have not been proven to belong to this call and cannot be rolled back.
func failedWorktreeAdd(ctx context.Context, root, path, branch string, created bool) CreateWorktreeResult {
	result := creationResult(ctx, root, path, branch, created)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		result.Path, result.ownedPath = "", ""
	}
	if created {
		if exists, err := localBranchExists(ctx, root, branch); err == nil && !exists {
			result.Branch, result.ownedBranch, result.BranchCreated, result.ownedBranchCreated = "", "", false, false
		}
	} else if result.Path == "" {
		result.Branch, result.ownedBranch = "", ""
	}
	return result
}

func (r CreateWorktreeResult) creationFailure(ctx context.Context, policy FailureCleanup, cause error) (CreateWorktreeResult, error) {
	if policy == CleanupDeferred {
		return r, cause
	}
	_, cleanupErr := rollbackCreatedWorktreeWithResult(context.WithoutCancel(ctx), r.projectRoot, r.ownedPath, r.ownedBranch, r.ownedBranchCreated)
	return r, errors.Join(cause, cleanupErr)
}

func snapshotAcquisition(ctx context.Context, r CreateWorktreeResult) (CreateWorktreeResult, error) {
	e := newLifecycleExecution(r.runner, r.runGit, r.runHook)
	registration, err := e.run(ctx, r.ownedPath, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return r, err
	}
	r.registration = filepath.Clean(strings.TrimSpace(string(registration)))
	r.pathInfo, err = os.Lstat(r.ownedPath)
	if err != nil || !r.pathInfo.IsDir() {
		return r, fmt.Errorf("capture worktree directory: %w", errors.Join(err, ErrWorktreeCleanupIncomplete))
	}
	r.registrationInfo, err = os.Stat(r.registration)
	if err != nil {
		return r, err
	}
	r.verified = true
	return r, nil
}
