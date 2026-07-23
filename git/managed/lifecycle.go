package managedworktree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitworktree "go.kenn.io/kit/git/worktree"
	"go.kenn.io/kit/safefileio"
)

// Sentinel errors for worktree lifecycle failures the HTTP layer maps to
// distinct problem codes. They are wrapped with operation detail; match
// with errors.Is.
var (
	// ErrWorktreeDestinationExists reports that the worktree target path
	// already exists on disk or is already used by another worktree.
	ErrWorktreeDestinationExists = errors.New(
		"worktree destination already exists",
	)
	// ErrBranchInUse reports that the branch is checked out in another
	// worktree, so it can be neither attached nor deleted.
	ErrBranchInUse = errors.New(
		"branch is checked out in another worktree",
	)
	// ErrInvalidBranchName reports a branch name git rejects
	// (`git check-ref-format --branch`).
	ErrInvalidBranchName = errors.New("invalid branch name")
	// ErrHookOutsideProject reports a lifecycle hook script path that
	// resolves outside the project tree. Hooks are arbitrary executables;
	// confining them to the project keeps a registry entry from running
	// code elsewhere on the machine.
	ErrHookOutsideProject = errors.New(
		"lifecycle hook script resolves outside the project",
	)
	// ErrWorktreeCleanupIncomplete reports that destructive cleanup was not
	// attempted because the operation could not establish or revalidate
	// ownership of the worktree artifacts.
	ErrWorktreeCleanupIncomplete = errors.New("worktree cleanup incomplete")
)

// HookError reports a lifecycle hook script that ran and exited non-zero.
type HookError struct {
	Script   string
	ExitCode int
	Stderr   string
}

// GitRunner runs one Git command under an application's process policy.
type GitRunner func(
	ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
) ([]byte, error)

// HookCommand describes one lifecycle hook invocation.
type HookCommand struct {
	Script string
	Dir    string
	Env    []string
	Stdout io.Writer
	Stderr io.Writer
}

// HookRunner runs one lifecycle hook under an application's process policy.
type HookRunner func(context.Context, HookCommand) error

func (e *HookError) Error() string {
	return fmt.Sprintf(
		"%s failed with exit code %d: %s", e.Script, e.ExitCode, e.Stderr,
	)
}

const defaultHookEnvironmentPrefix = "KIT"

// CreateWorktreeOptions parameterizes CreateWorktreeOnDisk. ProjectRoot and
// Branch are required; everything else is optional. Lifecycle script paths
// arrive per call: the caller owns config sourcing (project files, app
// settings) and this package owns execution.
type CreateWorktreeOptions struct {
	// ProjectRoot is the repository checkout git commands run in.
	ProjectRoot string
	// Branch is the branch to attach or create.
	Branch string
	// Path is the worktree destination. When empty it derives from
	// BaseDir (default "<ProjectRoot>-worktrees") plus the slash-slugged
	// branch name.
	Path string
	// BaseDir overrides the derivation base used when Path is empty. The
	// default base is restricted to the current user; an explicit BaseDir is
	// caller-owned and is the opt-in for a shared or otherwise custom base.
	BaseDir string
	// BaseRef, when set, forces creation of a new Branch starting at this
	// ref (git worktree add <path> -b <branch> -- <ref>). When empty, an
	// existing local Branch is attached and a missing one is created from
	// HEAD.
	BaseRef string
	// SetupScript, when set, runs in the new worktree after git work
	// succeeds. Relative paths resolve against ProjectRoot; the resolved
	// path must stay inside the project tree. A non-zero exit rolls the
	// worktree (and any branch this call created) back.
	SetupScript string
	// WorktreeName is the display name exported to hook scripts; defaults
	// to Branch.
	WorktreeName string
	// HookEnvironmentPrefix prefixes WORKTREE_NAME, WORKTREE_PATH,
	// PROJECT_ROOT, and BRANCH in the hook environment. It defaults to KIT.
	HookEnvironmentPrefix string
	// Runner overrides the Git execution policy. A zero runner preserves the
	// production lifecycle defaults.
	Runner gitcmd.Runner
	// RunGit and RunHook let an application retain its process limiter and
	// platform-specific command policy. Their zero values execute directly.
	RunGit  GitRunner
	RunHook HookRunner
	// IsolatedCheckout prepares the worktree without checkout, calls
	// BeforeCheckout, then materializes files with hooks, filters, and fsmonitor
	// disabled.
	IsolatedCheckout bool
	// NoTrack prevents Git from implicitly configuring upstream tracking when
	// BaseRef names a remote-tracking branch.
	NoTrack        bool
	BeforeCheckout func(context.Context, string) error
}

// CreateWorktreeResult reports what CreateWorktreeOnDisk did.
type CreateWorktreeResult struct {
	Path   string
	Branch string
	// BranchCreated reports whether this call created the branch (as
	// opposed to attaching a pre-existing local branch). Callers rolling
	// the git work back must use this result's Rollback method so a
	// pre-existing branch is never force-deleted.
	BranchCreated bool
	HookRan       bool
	HookScript    string
	projectRoot   string
	runner        gitcmd.Runner
	runGit        GitRunner
	runHook       HookRunner
	pathInfo      os.FileInfo
	branchOID     string
	headOID       string
	headRef       string
	materialized  bool
	snapshotted   bool
}

// RollbackResult identifies worktree artifacts that remained after rollback.
type RollbackResult struct {
	Path   string
	Branch string
}

// Rollback unwinds the worktree represented by this creation result.
func (r CreateWorktreeResult) Rollback(ctx context.Context) (RollbackResult, error) {
	if !r.snapshotted {
		remaining := RollbackResult{Path: r.Path}
		if r.BranchCreated {
			remaining.Branch = r.Branch
		}
		return remaining, fmt.Errorf(
			"%w: ownership snapshot unavailable; preserving worktree artifacts",
			ErrWorktreeCleanupIncomplete,
		)
	}
	ctx = withLifecycleExecution(ctx, r.runner, r.runGit, r.runHook)
	ctx, unlock, err := acquireRepositoryMutationLock(ctx, r.projectRoot)
	if err != nil {
		return RollbackResult{Path: r.Path, Branch: r.Branch}, err
	}
	remaining, rollbackErr := r.rollbackOwned(ctx, true)
	return remaining, errors.Join(rollbackErr, unlock())
}

// CreateWorktreeOnDisk performs the git side of worktree creation: it
// derives and validates the destination, runs `git worktree add`
// (attaching an existing branch or creating a new one), and runs the
// optional setup hook. On hook failure the worktree — and the branch this
// call created — are rolled back so a retry does not trip
// ErrWorktreeDestinationExists.
func CreateWorktreeOnDisk(
	ctx context.Context, opts CreateWorktreeOptions,
) (CreateWorktreeResult, error) {
	root, err := absRequired(opts.ProjectRoot, "project root")
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	ctx, unlock, err := acquireRepositoryMutationLock(ctx, root)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	result, createErr := createWorktreeOnDisk(ctx, opts)
	return result, errors.Join(createErr, unlock())
}

func createWorktreeOnDisk(
	ctx context.Context, opts CreateWorktreeOptions,
) (CreateWorktreeResult, error) {
	ctx = withLifecycleExecution(ctx, opts.Runner, opts.RunGit, opts.RunHook)
	root, branch, err := requireRootAndBranch(
		opts.ProjectRoot, opts.Branch,
	)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	if opts.IsolatedCheckout {
		if err := validateIsolatedCheckoutGitVersion(ctx, root); err != nil {
			return CreateWorktreeResult{}, err
		}
	}
	if err := validateBranchName(ctx, root, branch); err != nil {
		return CreateWorktreeResult{}, err
	}
	hookScript, err := resolveHookScript(root, opts.SetupScript)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	path, err := resolveWorktreeDestination(
		root, branch, opts.Path, opts.BaseDir,
	)
	if err != nil {
		return CreateWorktreeResult{}, err
	}

	branchExisted := localBranchExists(ctx, root, branch)
	var args []string
	switch {
	case opts.BaseRef != "":
		// Double-dash keeps a ref-like branch argument from being
		// parsed as a path and vice versa.
		args = []string{
			"worktree", "add", path, "-b", branch, "--", opts.BaseRef,
		}
	case branchExisted:
		args = []string{"worktree", "add", path, branch}
	default:
		args = []string{"worktree", "add", "-b", branch, path}
	}
	if opts.IsolatedCheckout {
		args = slices.Insert(args, 2, "--no-checkout")
	}
	if opts.NoTrack {
		args = slices.Insert(args, 2, "--no-track")
	}
	var out []byte
	if opts.IsolatedCheckout {
		runner, runnerErr := lifecycleHooksRunner(ctx)
		if runnerErr != nil {
			return CreateWorktreeResult{}, runnerErr
		}
		out, err = runLifecycleGitWithRunner(ctx, runner, root, args...)
	} else {
		out, err = runLifecycleGit(ctx, root, args...)
	}
	if err != nil {
		return CreateWorktreeResult{}, classifyWorktreeGitError(out, err)
	}

	result, err := snapshotCreateWorktreeResult(
		ctx, root, path, branch, !branchExisted, !opts.IsolatedCheckout,
	)
	if err != nil {
		return result, errors.Join(err, fmt.Errorf(
			"%w: ownership snapshot unavailable; preserving worktree at %s",
			ErrWorktreeCleanupIncomplete, path,
		))
	}
	if opts.IsolatedCheckout {
		if opts.BeforeCheckout != nil {
			if err := opts.BeforeCheckout(ctx, path); err != nil {
				_, cleanupErr := result.rollbackOwned(context.WithoutCancel(ctx), true)
				return result, errors.Join(fmt.Errorf("pre-checkout validation: %w", err), cleanupErr)
			}
		}
		result.materialized = true
		if err := checkoutIsolated(ctx, path); err != nil {
			_, cleanupErr := result.Rollback(context.WithoutCancel(ctx))
			return result, errors.Join(err, cleanupErr)
		}
	}
	if hookScript.path != "" {
		hookErr := runLifecycleHook(
			ctx, hookScript, root, path, branch, opts.WorktreeName,
			opts.HookEnvironmentPrefix,
		)
		if hookErr != nil {
			_, cleanupErr := result.rollbackOwned(
				context.WithoutCancel(ctx), false,
			)
			return result, errors.Join(hookErr, cleanupErr)
		}
		result.HookRan = true
		result.HookScript = hookScript.requested
	}
	return result, nil
}

func validateIsolatedCheckoutGitVersion(ctx context.Context, root string) error {
	output, err := runLifecycleGit(ctx, root, "version")
	if err != nil {
		return fmt.Errorf("determine Git version for isolated checkout: %w", err)
	}
	if !supportsChangeRequestGitVersion(string(output), runtime.GOOS) {
		return errors.New("isolated checkout requires " + safeCheckoutGitVersionRequirement(runtime.GOOS))
	}
	return nil
}

func snapshotCreateWorktreeResult(
	ctx context.Context,
	root, path, branch string,
	branchCreated, materialized bool,
) (CreateWorktreeResult, error) {
	result := CreateWorktreeResult{
		Path: path, Branch: branch, BranchCreated: branchCreated,
		projectRoot: root, runner: lifecycleRunner(ctx),
		runGit: lifecycleGitRunner(ctx), runHook: lifecycleHookRunner(ctx),
		materialized: materialized,
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return result, fmt.Errorf("inspect created worktree path: %w", err)
	}
	result.pathInfo = pathInfo
	out, err := runLifecycleGit(ctx, root, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return result, fmt.Errorf("resolve created worktree branch: %w", err)
	}
	result.branchOID = strings.TrimSpace(string(out))
	headRef, headOID, err := lifecycleWorktreeHead(ctx, path)
	if err != nil {
		return result, fmt.Errorf("resolve created worktree HEAD: %w", err)
	}
	result.headOID = headOID
	result.headRef = headRef
	result.snapshotted = true
	return result, nil
}

func (r CreateWorktreeResult) rollbackOwned(
	ctx context.Context, preserveChanges bool,
) (RollbackResult, error) {
	remaining := RollbackResult{}
	var errs []error
	preserveBranch := false
	pathOwned := sameLifecycleFile(r.Path, r.pathInfo)
	pathPresent := lifecyclePathExists(r.Path)
	branchOID, branchExists, branchDirect, branchErr := lifecycleRefState(ctx, r.projectRoot, r.Branch)
	if branchErr != nil {
		errs = append(errs, fmt.Errorf("inspect created branch: %w", branchErr))
	}
	var headRef, headOID string
	var headErr error
	if pathPresent && pathOwned {
		headRef, headOID, headErr = lifecycleWorktreeHead(ctx, r.Path)
	}

	switch {
	case pathPresent && !pathOwned:
		remaining.Path = r.Path
		errs = append(errs, errors.New("created worktree path ownership changed; preserving it"))
	case !pathPresent:
		preserveBranch = true
		errs = append(errs, errors.New("created worktree path disappeared; preserving its branch"))
	case headErr != nil:
		remaining.Path = r.Path
		errs = append(errs, fmt.Errorf("inspect created worktree HEAD: %w", headErr))
	case headRef != r.headRef || !strings.EqualFold(headOID, r.headOID):
		remaining.Path = r.Path
		errs = append(errs, errors.New("created worktree HEAD changed; preserving it"))
	case branchErr != nil || branchExists &&
		(!branchDirect || !strings.EqualFold(branchOID, r.branchOID)):
		remaining.Path = r.Path
		errs = append(errs, errors.New("created worktree branch advanced; preserving it"))
	case !r.materialized && preserveChanges:
		hasArtifacts, err := unmaterializedWorktreeHasArtifacts(r.Path)
		if err != nil {
			remaining.Path = r.Path
			errs = append(errs, fmt.Errorf("inspect unmaterialized worktree: %w", err))
		} else if hasArtifacts {
			remaining.Path = r.Path
			errs = append(errs, errors.New("unmaterialized worktree contains unexpected artifacts; preserving it"))
		}
	case preserveChanges:
		runner, err := isolatedLifecycleRunner(ctx, r.Path)
		if err != nil {
			remaining.Path = r.Path
			errs = append(errs, fmt.Errorf("inspect rollback filters: %w", err))
			break
		}
		status, err := runLifecycleGitWithRunner(
			ctx, runner, r.Path,
			"status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching",
		)
		if err != nil {
			remaining.Path = r.Path
			errs = append(errs, fmt.Errorf("inspect created worktree changes: %w", err))
		} else if strings.TrimSpace(string(status)) != "" {
			remaining.Path = r.Path
			errs = append(errs, errors.New("created worktree contains changes; preserving it"))
		}
	}

	if remaining.Path == "" && pathOwned {
		runner, err := lifecycleHooksRunner(ctx)
		if err != nil {
			remaining.Path = r.Path
			errs = append(errs, err)
		} else if err := verifyRegisteredWorktree(
			ctx, r.projectRoot, r.Path, r.Branch, r.pathInfo,
		); err != nil {
			remaining.Path = r.Path
			errs = append(errs, err)
		} else if out, err := runLifecycleGitWithRunner(
			ctx, runner, r.projectRoot,
			"worktree", "remove", "--force", r.Path,
		); err != nil {
			remaining.Path = r.Path
			errs = append(errs, fmt.Errorf("remove created worktree: %w: %s", err,
				strings.TrimSpace(string(out))))
		}
	}

	if r.BranchCreated {
		if !preserveBranch && remaining.Path == "" && branchErr == nil && branchExists && strings.EqualFold(branchOID, r.branchOID) {
			currentOID, currentExists, currentDirect, inspectErr := lifecycleRefState(
				ctx, r.projectRoot, r.Branch,
			)
			switch {
			case inspectErr != nil:
				errs = append(errs, fmt.Errorf("revalidate created branch: %w", inspectErr))
			case currentExists && (!currentDirect || !strings.EqualFold(currentOID, r.branchOID)):
				errs = append(errs, errors.New("created worktree branch ownership changed; preserving it"))
			case currentExists:
				runner, err := lifecycleHooksRunner(ctx)
				if err != nil {
					errs = append(errs, err)
				} else if out, err := runLifecycleGitWithRunner(
					ctx, runner, r.projectRoot,
					"update-ref", "--no-deref", "-d", "refs/heads/"+r.Branch, r.branchOID,
				); err != nil {
					errs = append(errs, fmt.Errorf("delete created branch: %w: %s", err,
						strings.TrimSpace(string(out))))
				}
			}
		}
		_, stillExists, _, err := lifecycleRefState(ctx, r.projectRoot, r.Branch)
		if err != nil {
			remaining.Branch = r.Branch
			errs = append(errs, fmt.Errorf("inspect created branch after rollback: %w", err))
		} else if stillExists {
			remaining.Branch = r.Branch
		}
	}
	if remaining.Path != "" {
		errs = append(errs, ErrWorktreeCleanupIncomplete)
		errs = append(errs, errors.New("created worktree path remains"))
	} else if remaining.Branch != "" {
		errs = append(errs, ErrWorktreeCleanupIncomplete)
	}
	if remaining.Branch != "" {
		errs = append(errs, errors.New("created worktree branch remains"))
	}
	return remaining, errors.Join(errs...)
}

func unmaterializedWorktreeHasArtifacts(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() != ".git" {
			return true, nil
		}
	}
	return false, nil
}

func lifecycleRefState(
	ctx context.Context, root, branch string,
) (oid string, exists, direct bool, err error) {
	refName := "refs/heads/" + branch
	out, err := runLifecycleGit(
		ctx, root, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)", refName,
	)
	if err != nil {
		return "", false, false, err
	}
	record := strings.TrimSpace(string(out))
	if record == "" {
		return "", false, false, nil
	}
	fields := strings.Split(record, "\x00")
	if len(fields) != 3 || fields[0] != refName {
		return "", false, false, fmt.Errorf("unexpected ref inspection output")
	}
	return strings.TrimSpace(fields[1]), true, strings.TrimSpace(fields[2]) == "", nil
}

func lifecycleWorktreeHead(ctx context.Context, path string) (string, string, error) {
	runner, err := isolatedLifecycleRunner(ctx, path)
	if err != nil {
		return "", "", err
	}
	oidOutput, err := runLifecycleGitWithRunner(
		ctx, runner, path, "rev-parse", "--verify", "HEAD^{commit}",
	)
	if err != nil {
		return "", "", err
	}
	refOutput, err := runLifecycleGitWithRunner(
		ctx, runner, path, "rev-parse", "--symbolic-full-name", "HEAD",
	)
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(string(refOutput)), strings.TrimSpace(string(oidOutput)), nil
}

func sameLifecycleFile(path string, expected os.FileInfo) bool {
	if expected == nil {
		return false
	}
	current, err := os.Lstat(path)
	return err == nil && os.SameFile(expected, current)
}

func lifecyclePathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !os.IsNotExist(err)
}

// RemoveWorktreeOptions parameterizes RemoveWorktreeFromDisk. ProjectRoot
// and Path are required.
type RemoveWorktreeOptions struct {
	ProjectRoot string
	Path        string
	// Branch is the branch deleted when RemoveBranch is set; an empty
	// branch (detached HEAD) makes RemoveBranch a no-op.
	Branch string
	// Force passes --force twice to git worktree remove so dirty and locked
	// worktrees still go. Policy checks (refusing dirty removal without force)
	// belong to the caller.
	Force        bool
	RemoveBranch bool
	// TeardownScript, when set, runs in the worktree before removal.
	// Relative paths resolve against ProjectRoot and must stay inside the
	// project tree. A non-zero exit aborts the removal. The hook is
	// skipped when the worktree path is already gone.
	TeardownScript string
	// WorktreeName is the display name exported to hook scripts; defaults
	// to Branch.
	WorktreeName          string
	HookEnvironmentPrefix string
	Runner                gitcmd.Runner
	RunGit                GitRunner
	RunHook               HookRunner
}

// RemoveWorktreeResult reports what RemoveWorktreeFromDisk did.
type RemoveWorktreeResult struct {
	HookRan    bool
	HookScript string
}

// RemoveWorktreeFromDisk performs the git side of worktree removal: it
// runs the optional teardown hook, removes the worktree (or its exact stale
// registration when the path is already gone), and optionally
// deletes the branch.
func RemoveWorktreeFromDisk(
	ctx context.Context, opts RemoveWorktreeOptions,
) (RemoveWorktreeResult, error) {
	root, err := absRequired(opts.ProjectRoot, "project root")
	if err != nil {
		return RemoveWorktreeResult{}, err
	}
	ctx, unlock, err := acquireRepositoryMutationLock(ctx, root)
	if err != nil {
		return RemoveWorktreeResult{}, err
	}
	result, removeErr := removeWorktreeFromDisk(ctx, opts)
	return result, errors.Join(removeErr, unlock())
}

func removeWorktreeFromDisk(
	ctx context.Context, opts RemoveWorktreeOptions,
) (RemoveWorktreeResult, error) {
	ctx = withLifecycleExecution(ctx, opts.Runner, opts.RunGit, opts.RunHook)
	root, err := absRequired(opts.ProjectRoot, "project root")
	if err != nil {
		return RemoveWorktreeResult{}, err
	}
	path, err := absRequired(opts.Path, "worktree path")
	if err != nil {
		return RemoveWorktreeResult{}, err
	}
	pathExists := true
	var pathInfo os.FileInfo
	if pathInfo, err = os.Lstat(path); err != nil {
		if !os.IsNotExist(err) {
			return RemoveWorktreeResult{}, fmt.Errorf(
				"stat worktree path: %w", err,
			)
		}
		pathExists = false
		pathInfo = nil
	}
	if err := verifyRegisteredWorktree(ctx, root, path, opts.Branch, pathInfo); err != nil {
		return RemoveWorktreeResult{}, err
	}
	hookScript := lifecycleHookScript{}
	if pathExists {
		hookScript, err = resolveHookScript(root, opts.TeardownScript)
		if err != nil {
			return RemoveWorktreeResult{}, err
		}
	}

	result := RemoveWorktreeResult{}
	if hookScript.path != "" && pathExists {
		if err := verifyRegisteredWorktree(ctx, root, path, opts.Branch, pathInfo); err != nil {
			return RemoveWorktreeResult{}, err
		}
		if hookErr := runLifecycleHook(
			ctx, hookScript, root, path, opts.Branch, opts.WorktreeName,
			opts.HookEnvironmentPrefix,
		); hookErr != nil {
			return RemoveWorktreeResult{}, hookErr
		}
		result.HookRan = true
		result.HookScript = hookScript.requested
	}

	if pathExists {
		if err := verifyRegisteredWorktree(ctx, root, path, opts.Branch, pathInfo); err != nil {
			return result, err
		}
		args := []string{"worktree", "remove"}
		if opts.Force {
			args = append(args, "--force", "--force")
		}
		args = append(args, path)
		if out, err := runLifecycleGit(ctx, root, args...); err != nil {
			return result, classifyWorktreeGitError(out, err)
		}
	} else {
		// The directory is gone but git may still hold a stale
		// registration that would block branch deletion and re-creation.
		// Removing by path leaves unrelated stale registrations alone.
		if _, statErr := os.Lstat(path); statErr == nil {
			return result, fmt.Errorf("%w: worktree path appeared during removal; preserving it",
				ErrWorktreeCleanupIncomplete)
		} else if !os.IsNotExist(statErr) {
			return result, fmt.Errorf("recheck missing worktree path: %w", statErr)
		}
		args := []string{"worktree", "remove", "--force"}
		if opts.Force {
			args = append(args, "--force")
		}
		args = append(args, path)
		if out, err := runLifecycleGit(ctx, root, args...); err != nil {
			return result, classifyWorktreeGitError(out, err)
		}
	}

	if opts.RemoveBranch && strings.TrimSpace(opts.Branch) != "" {
		if out, err := runLifecycleGit(
			ctx, root, "branch", "-D", "--", opts.Branch,
		); err != nil {
			return result, classifyWorktreeGitError(out, err)
		}
	}
	return result, nil
}

func verifyRegisteredWorktree(
	ctx context.Context, root, path, branch string, pathInfo os.FileInfo,
) error {
	runner, err := lifecycleHooksRunner(ctx)
	if err != nil {
		return err
	}
	out, err := runLifecycleGitWithRunner(
		ctx, runner, root, "worktree", "list", "--porcelain",
	)
	if err != nil {
		return fmt.Errorf("%w: inspect registered worktrees: %w",
			ErrWorktreeCleanupIncomplete, err)
	}
	wantPath := canonicalLifecyclePath(path)
	wantBranch := strings.TrimSpace(branch)
	var registeredEntry *gitworktree.PorcelainEntry
	for _, entry := range gitworktree.ParsePorcelain(string(out)) {
		if !lifecyclePathsEqual(entry.Path, wantPath) {
			continue
		}
		entryCopy := entry
		registeredEntry = &entryCopy
		if wantBranch != "" && entry.Branch != wantBranch {
			return fmt.Errorf("%w: registered worktree branch changed; preserving path",
				ErrWorktreeCleanupIncomplete)
		}
		if wantBranch == "" && !entry.Detached {
			return fmt.Errorf("%w: registered worktree is no longer detached; preserving path",
				ErrWorktreeCleanupIncomplete)
		}
		break
	}
	if registeredEntry == nil {
		return fmt.Errorf("%w: path is no longer a registered worktree; preserving it",
			ErrWorktreeCleanupIncomplete)
	}
	if pathInfo == nil {
		return nil
	}
	if !sameLifecycleFile(path, pathInfo) {
		return fmt.Errorf("%w: registered worktree path identity changed; preserving it",
			ErrWorktreeCleanupIncomplete)
	}
	topLevel, err := runLifecycleGitWithRunner(
		ctx, runner, path, "rev-parse", "--path-format=absolute", "--show-toplevel",
	)
	if err != nil || !lifecyclePathsEqual(strings.TrimSpace(string(topLevel)), wantPath) {
		return fmt.Errorf("%w: path no longer resolves to the registered worktree; preserving it",
			ErrWorktreeCleanupIncomplete)
	}
	rootCommon, rootErr := runLifecycleGitWithRunner(
		ctx, runner, root, "rev-parse", "--path-format=absolute", "--git-common-dir",
	)
	pathCommon, pathErr := runLifecycleGitWithRunner(
		ctx, runner, path, "rev-parse", "--path-format=absolute", "--git-common-dir",
	)
	if rootErr != nil || pathErr != nil || !lifecyclePathsEqual(
		strings.TrimSpace(string(rootCommon)), strings.TrimSpace(string(pathCommon)),
	) {
		return fmt.Errorf("%w: worktree repository identity changed; preserving path",
			ErrWorktreeCleanupIncomplete)
	}
	actualBranch, branchErr := runLifecycleGitWithRunner(
		ctx, runner, path, "rev-parse", "--abbrev-ref", "HEAD",
	)
	if branchErr != nil {
		return fmt.Errorf("%w: failed to inspect worktree HEAD identity; preserving path",
			ErrWorktreeCleanupIncomplete)
	}
	actualBranchName := strings.TrimSpace(string(actualBranch))
	if wantBranch != "" {
		if actualBranchName != wantBranch {
			return fmt.Errorf("%w: worktree HEAD branch changed; preserving path",
				ErrWorktreeCleanupIncomplete)
		}
	} else if actualBranchName != "HEAD" {
		return fmt.Errorf("%w: worktree HEAD is no longer detached; preserving path",
			ErrWorktreeCleanupIncomplete)
	}
	return nil
}

func canonicalLifecyclePath(path string) string {
	clean := filepath.Clean(strings.TrimSpace(path))
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(resolved)
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(clean)); err == nil {
		return filepath.Join(parent, filepath.Base(clean))
	}
	return clean
}

func lifecyclePathsEqual(left, right string) bool {
	left = canonicalLifecyclePath(left)
	right = canonicalLifecyclePath(right)
	if left == right || runtime.GOOS == "windows" && strings.EqualFold(left, right) {
		return true
	}
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

// WorktreeIsDirty reports whether the worktree at path has uncommitted
// changes (staged, unstaged, or untracked).
func WorktreeIsDirty(ctx context.Context, path string) (bool, error) {
	runner, err := isolatedLifecycleRunner(ctx, path)
	if err != nil {
		return false, fmt.Errorf("inspect worktree filters: %w", err)
	}
	out, err := runLifecycleGitWithRunner(
		ctx, runner, path, "status", "--porcelain", "--untracked-files=all",
	)
	if err != nil {
		return false, fmt.Errorf(
			"check worktree dirty state: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func requireRootAndBranch(
	rawRoot, rawBranch string,
) (string, string, error) {
	root, err := absRequired(rawRoot, "project root")
	if err != nil {
		return "", "", err
	}
	branch := strings.TrimSpace(rawBranch)
	if branch == "" {
		return "", "", fmt.Errorf("branch is required")
	}
	return root, branch, nil
}

func absRequired(raw, label string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	return abs, nil
}

func validateBranchName(
	ctx context.Context, root, branch string,
) error {
	if _, err := runLifecycleGit(
		ctx, root, "check-ref-format", "--branch", branch,
	); err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidBranchName, branch)
	}
	return nil
}

type lifecycleHookScript struct {
	requested string
	path      string
	info      os.FileInfo
	digest    [sha256.Size]byte
}

// resolveHookScript resolves a caller-supplied hook script path against the
// project root and snapshots the existing regular file's identity. Requiring
// the target to exist before worktree mutation prevents a dangling symlink
// from becoming contributor-controlled after checkout.
func resolveHookScript(projectRoot, raw string) (lifecycleHookScript, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return lifecycleHookScript{}, nil
	}
	requested := trimmed
	if !filepath.IsAbs(requested) {
		requested = filepath.Join(projectRoot, requested)
	}
	requested = filepath.Clean(requested)
	canonicalRoot, err := filepath.EvalSymlinks(projectRoot)
	if err != nil {
		return lifecycleHookScript{}, fmt.Errorf("resolve lifecycle hook project root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(requested)
	if err != nil {
		if !pathWithinRoot(filepath.Clean(projectRoot), requested) {
			return lifecycleHookScript{}, fmt.Errorf("%w: %q", ErrHookOutsideProject, raw)
		}
		return lifecycleHookScript{}, fmt.Errorf("resolve lifecycle hook script: %w", err)
	}
	if !pathWithinRoot(canonicalRoot, resolved) {
		return lifecycleHookScript{}, fmt.Errorf("%w: %q", ErrHookOutsideProject, raw)
	}
	info, digest, _, err := snapshotLifecycleHook(resolved)
	if err != nil {
		return lifecycleHookScript{}, fmt.Errorf("inspect lifecycle hook script: %w", err)
	}
	return lifecycleHookScript{
		requested: requested, path: resolved, info: info, digest: digest,
	}, nil
}

func (h lifecycleHookScript) verifiedContents(projectRoot string) ([]byte, error) {
	canonicalRoot, err := filepath.EvalSymlinks(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("revalidate lifecycle hook project root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(h.requested)
	if err != nil {
		return nil, fmt.Errorf("revalidate lifecycle hook script: %w", err)
	}
	if resolved != h.path || !pathWithinRoot(canonicalRoot, resolved) {
		return nil, errors.New("lifecycle hook script target changed before execution")
	}
	info, digest, contents, err := snapshotLifecycleHook(resolved)
	if err != nil {
		return nil, fmt.Errorf("revalidate lifecycle hook script: %w", err)
	}
	if !os.SameFile(h.info, info) || info.Mode() != h.info.Mode() || digest != h.digest {
		return nil, errors.New("lifecycle hook script identity changed before execution")
	}
	return contents, nil
}

func snapshotLifecycleHook(path string) (os.FileInfo, [sha256.Size]byte, []byte, error) {
	var digest [sha256.Size]byte
	file, err := os.Open(path)
	if err != nil {
		return nil, digest, nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, digest, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, digest, nil, fmt.Errorf("%s is not a regular file", path)
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		return nil, digest, nil, err
	}
	digest = sha256.Sum256(contents)
	return info, digest, contents, nil
}

func (h lifecycleHookScript) executableSnapshot(contents []byte) (string, func(), error) {
	dir, err := lifecycleHooksDir()
	if err != nil {
		return "", nil, fmt.Errorf("prepare lifecycle hook snapshot: %w", err)
	}
	file, err := os.CreateTemp(dir, "lifecycle-*"+filepath.Ext(h.path))
	if err != nil {
		return "", nil, fmt.Errorf("create lifecycle hook snapshot: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	fail := func(cause error) (string, func(), error) {
		_ = file.Close()
		cleanup()
		return "", nil, cause
	}
	if err := file.Chmod(lifecycleHookSnapshotMode(runtime.GOOS)); err != nil {
		return fail(fmt.Errorf("secure lifecycle hook snapshot: %w", err))
	}
	if _, err := file.Write(contents); err != nil {
		return fail(fmt.Errorf("write lifecycle hook snapshot: %w", err))
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close lifecycle hook snapshot: %w", err)
	}
	return path, cleanup, nil
}

func lifecycleHookSnapshotMode(goos string) os.FileMode {
	if goos == "windows" {
		// On Windows, removing a read-only file fails. The containing hooks
		// directory is private, so retaining owner write permission keeps the
		// immutable snapshot confined while allowing deterministic cleanup.
		return 0o700
	}
	return 0o500
}

func pathWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveWorktreeDestination returns the validated absolute worktree
// destination. An explicit path wins; otherwise the destination derives
// from baseDir (default "<root>-worktrees") plus the slash-slugged branch.
// The destination must not already exist.
func resolveWorktreeDestination(
	root, branch, explicitPath, baseDir string,
) (string, error) {
	var dest string
	if strings.TrimSpace(explicitPath) != "" {
		abs, err := filepath.Abs(strings.TrimSpace(explicitPath))
		if err != nil {
			return "", fmt.Errorf("resolve worktree path: %w", err)
		}
		dest = abs
	} else {
		base := strings.TrimSpace(baseDir)
		privateBase := base == ""
		if privateBase {
			base = root + "-worktrees"
		}
		var err error
		if privateBase {
			err = safefileio.EnsurePrivateDir(base)
		} else {
			err = os.MkdirAll(base, 0o755)
		}
		if err != nil {
			return "", fmt.Errorf("create worktree base dir: %w", err)
		}
		// Canonicalize the base so derived paths agree with what git
		// and discovery report (macOS /tmp vs /private/tmp).
		if resolved, err := filepath.EvalSymlinks(base); err == nil {
			base = resolved
		}
		slug := strings.ReplaceAll(branch, "/", "-")
		abs, err := filepath.Abs(filepath.Join(base, slug))
		if err != nil {
			return "", fmt.Errorf("resolve worktree path: %w", err)
		}
		dest = abs
	}
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf(
			"%w: %s", ErrWorktreeDestinationExists, dest,
		)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat worktree destination: %w", err)
	}
	return dest, nil
}

func localBranchExists(ctx context.Context, root, branch string) bool {
	_, err := runLifecycleGit(
		ctx, root, "show-ref", "--verify", "--quiet",
		"refs/heads/"+branch,
	)
	return err == nil
}

func runLifecycleGit(
	ctx context.Context, dir string, args ...string,
) ([]byte, error) {
	return runLifecycleGitWithRunner(ctx, lifecycleRunner(ctx), dir, args...)
}

func runLifecycleGitWithRunner(
	ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
) ([]byte, error) {
	if run := lifecycleGitRunner(ctx); run != nil {
		return run(ctx, runner, dir, args...)
	}
	stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
	return append(stdout, stderr...), err
}

type lifecycleExecutionContextKey struct{}

type lifecycleExecution struct {
	runner  gitcmd.Runner
	runGit  GitRunner
	runHook HookRunner
}

func withLifecycleExecution(
	ctx context.Context, runner gitcmd.Runner, runGit GitRunner, runHook HookRunner,
) context.Context {
	runner = normalizeLifecycleRunner(runner, gitcmd.Runner{
		Env: os.Environ(), StripEnv: true,
	})
	return context.WithValue(ctx, lifecycleExecutionContextKey{}, lifecycleExecution{
		runner: runner, runGit: runGit, runHook: runHook,
	})
}

func normalizeLifecycleRunner(runner, fallback gitcmd.Runner) gitcmd.Runner {
	if lifecycleRunnerIsZero(runner) {
		return fallback
	}
	if runner.Env == nil {
		runner.Env = os.Environ()
	}
	return runner
}

func lifecycleRunnerIsZero(runner gitcmd.Runner) bool {
	return runner.Env == nil && len(runner.Config) == 0 &&
		!runner.StripEnv && !runner.TerminalPrompt &&
		!runner.NullGlobalConfig && !runner.NoSystemConfig &&
		!runner.DisableSafeDirectoryForward
}

func lifecycleRunner(ctx context.Context) gitcmd.Runner {
	if execution, ok := ctx.Value(lifecycleExecutionContextKey{}).(lifecycleExecution); ok && execution.runner.Env != nil {
		return execution.runner
	}
	return gitcmd.Runner{Env: os.Environ(), StripEnv: true}
}

func lifecycleGitRunner(ctx context.Context) GitRunner {
	execution, _ := ctx.Value(lifecycleExecutionContextKey{}).(lifecycleExecution)
	return execution.runGit
}

func lifecycleHookRunner(ctx context.Context) HookRunner {
	execution, _ := ctx.Value(lifecycleExecutionContextKey{}).(lifecycleExecution)
	return execution.runHook
}

func checkoutIsolated(ctx context.Context, path string) error {
	runner, err := isolatedLifecycleRunner(ctx, path)
	if err != nil {
		return fmt.Errorf("inspect checkout filters: %w", err)
	}
	if _, err := runLifecycleGitWithRunner(
		ctx, runner, path, "reset", "--hard", "HEAD",
	); err != nil {
		return fmt.Errorf("materialize isolated worktree: %w", err)
	}
	return nil
}

func isolatedLifecycleRunner(ctx context.Context, path string) (gitcmd.Runner, error) {
	runner, err := lifecycleHooksRunner(ctx)
	if err != nil {
		return gitcmd.Runner{}, err
	}
	out, err := runLifecycleGitWithRunner(
		ctx, runner, path, "config", "--includes", "--null", "--list",
	)
	if err != nil {
		return gitcmd.Runner{}, err
	}
	runner = runner.WithConfig("core.fsmonitor", "false")
	for _, driver := range lifecycleFilterDrivers(string(out)) {
		prefix := "filter." + driver + "."
		runner = runner.WithConfig(prefix+"clean", "")
		runner = runner.WithConfig(prefix+"smudge", "")
		runner = runner.WithConfig(prefix+"process", "")
		runner = runner.WithConfig(prefix+"required", "false")
	}
	return runner, nil
}

func lifecycleHooksRunner(ctx context.Context) (gitcmd.Runner, error) {
	hooksDir, err := lifecycleHooksDir()
	if err != nil {
		return gitcmd.Runner{}, fmt.Errorf("create isolated hooks directory: %w", err)
	}
	return lifecycleRunner(ctx).WithConfig("core.hooksPath", hooksDir), nil
}

func lifecycleFilterDrivers(configOutput string) []string {
	drivers := map[string]struct{}{}
	for record := range strings.SplitSeq(configOutput, "\x00") {
		key, _, found := strings.Cut(record, "\n")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		lower := strings.ToLower(key)
		if !strings.HasPrefix(lower, "filter.") {
			continue
		}
		for _, suffix := range []string{".clean", ".smudge", ".process", ".required"} {
			if strings.HasSuffix(lower, suffix) {
				driver := key[len("filter.") : len(key)-len(suffix)]
				if driver != "" {
					drivers[driver] = struct{}{}
				}
			}
		}
	}
	result := make([]string, 0, len(drivers))
	for driver := range drivers {
		result = append(result, driver)
	}
	slices.Sort(result)
	return result
}

var (
	lifecycleHooksOnce sync.Once
	lifecycleHooksPath string
	lifecycleHooksErr  error
)

func lifecycleHooksDir() (string, error) {
	lifecycleHooksOnce.Do(func() {
		userID, err := safefileio.CurrentUserID()
		if err != nil {
			lifecycleHooksErr = err
			return
		}
		base := filepath.Join(os.TempDir(), "kit-managed-hooks-"+userID)
		if err := safefileio.EnsurePrivateDir(base); err != nil {
			lifecycleHooksErr = err
			return
		}
		lifecycleHooksPath, lifecycleHooksErr = os.MkdirTemp(base, "disabled-")
		if lifecycleHooksErr == nil {
			lifecycleHooksErr = safefileio.EnsurePrivateDir(lifecycleHooksPath)
		}
	})
	return lifecycleHooksPath, lifecycleHooksErr
}

// classifyWorktreeGitError maps well-known git stderr phrases onto the
// package sentinels so the HTTP layer can answer with distinct problem
// codes instead of a generic failure.
func classifyWorktreeGitError(out []byte, err error) error {
	detail := strings.TrimSpace(string(out))
	switch {
	// "'X' is already checked out at ..." (older git, worktree add),
	// "'X' is already used by worktree at ..." (worktree add),
	// "cannot delete branch 'X' used by worktree at ..." (branch -D).
	case strings.Contains(detail, "is already checked out at"),
		strings.Contains(detail, "used by worktree at"):
		return fmt.Errorf("%w: %s", ErrBranchInUse, detail)
	case strings.Contains(detail, "already exists"):
		return fmt.Errorf("%w: %s", ErrWorktreeDestinationExists, detail)
	}
	return fmt.Errorf("git: %w: %s", err, detail)
}

// runLifecycleHook executes a hook script in the worktree directory with the
// lifecycle environment while honoring ctx cancellation. Stdout is discarded;
// stderr is captured into the HookError a non-zero exit produces.
func runLifecycleHook(
	ctx context.Context,
	script lifecycleHookScript, projectRoot, worktreePath, branch, worktreeName,
	environmentPrefix string,
) error {
	contents, err := script.verifiedContents(projectRoot)
	if err != nil {
		return err
	}
	executable, cleanup, err := script.executableSnapshot(contents)
	if err != nil {
		return err
	}
	defer cleanup()
	name := strings.TrimSpace(worktreeName)
	if name == "" {
		name = branch
	}
	prefix := strings.TrimSpace(environmentPrefix)
	if prefix == "" {
		prefix = defaultHookEnvironmentPrefix
	}
	environment := append(
		os.Environ(),
		prefix+"_WORKTREE_NAME="+name,
		prefix+"_WORKTREE_PATH="+worktreePath,
		prefix+"_PROJECT_ROOT="+projectRoot,
		prefix+"_BRANCH="+branch,
	)
	var stderr bytes.Buffer
	command := HookCommand{
		Script: executable, Dir: worktreePath, Env: environment,
		Stdout: io.Discard, Stderr: &stderr,
	}
	if run := lifecycleHookRunner(ctx); run != nil {
		err = run(ctx, command)
	} else {
		cmd := lifecycleCommandContext(ctx, command.Script)
		cmd.Dir = command.Dir
		cmd.Env = command.Env
		cmd.Stdout = command.Stdout
		cmd.Stderr = command.Stderr
		err = gitcmd.RunProcessTreeCommand(cmd)
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &HookError{
				Script:   script.path,
				ExitCode: exitErr.ExitCode(),
				Stderr:   strings.TrimSpace(stderr.String()),
			}
		}
		return fmt.Errorf("run lifecycle hook %s: %w", script.path, err)
	}
	return nil
}
