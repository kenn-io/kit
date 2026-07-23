package managedworktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitworktree "go.kenn.io/kit/git/worktree"
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
	// ErrWorktreeCleanupIncomplete reports that rollback preserved a worktree
	// because it contains changes or its branch advanced after creation.
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
	// BaseDir overrides the derivation base used when Path is empty.
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
}

// CreateWorktreeResult reports what CreateWorktreeOnDisk did.
type CreateWorktreeResult struct {
	Path   string
	Branch string
	// BranchCreated reports whether this call created the branch (as
	// opposed to attaching a pre-existing local branch). Rollback uses it so
	// a pre-existing branch is never deleted.
	BranchCreated bool
	HookRan       bool
	HookScript    string
	projectRoot   string
	runner        gitcmd.Runner
	runGit        GitRunner
	runHook       HookRunner
	branchOID     string
	headOID       string
	headRef       string
}

// RollbackResult identifies worktree artifacts that remained after rollback.
type RollbackResult struct {
	Path   string
	Branch string
}

// Rollback unwinds the worktree represented by this creation result.
func (r CreateWorktreeResult) Rollback(ctx context.Context) (RollbackResult, error) {
	ctx = withLifecycleExecution(ctx, r.runner, r.runGit, r.runHook)
	return r.rollbackOwned(ctx)
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
	ctx = withLifecycleExecution(ctx, opts.Runner, opts.RunGit, opts.RunHook)
	root, branch, err := requireRootAndBranch(
		opts.ProjectRoot, opts.Branch,
	)
	if err != nil {
		return CreateWorktreeResult{}, err
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
	if out, err := runLifecycleGit(ctx, root, args...); err != nil {
		return CreateWorktreeResult{}, classifyWorktreeGitError(out, err)
	}

	result, err := snapshotCreateWorktreeResult(
		ctx, root, path, branch, !branchExisted,
	)
	if err != nil {
		_, cleanupErr := rollbackCreatedWorktreeWithResult(
			context.WithoutCancel(ctx), root, path, branch, !branchExisted,
		)
		return CreateWorktreeResult{}, errors.Join(err, cleanupErr)
	}
	if hookScript != "" {
		hookErr := runLifecycleHook(
			ctx, hookScript, root, path, branch, opts.WorktreeName,
			opts.HookEnvironmentPrefix,
		)
		if hookErr != nil {
			_, cleanupErr := rollbackCreatedWorktreeWithResult(
				context.WithoutCancel(ctx), root, path, branch, !branchExisted,
			)
			return result, errors.Join(hookErr, cleanupErr)
		}
		result.HookRan = true
		result.HookScript = hookScript
	}
	return result, nil
}

func snapshotCreateWorktreeResult(
	ctx context.Context,
	root, path, branch string,
	branchCreated bool,
) (CreateWorktreeResult, error) {
	out, err := runLifecycleGit(ctx, root, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return CreateWorktreeResult{}, fmt.Errorf("resolve created worktree branch: %w", err)
	}
	headOID, headRef, err := lifecycleWorktreeHead(ctx, path)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	return CreateWorktreeResult{
		Path: path, Branch: branch, BranchCreated: branchCreated,
		projectRoot: root, runner: lifecycleRunner(ctx),
		runGit: lifecycleGitRunner(ctx), runHook: lifecycleHookRunner(ctx),
		branchOID: strings.TrimSpace(string(out)),
		headOID:   headOID,
		headRef:   headRef,
	}, nil
}

func (r CreateWorktreeResult) rollbackOwned(ctx context.Context) (RollbackResult, error) {
	remaining := RollbackResult{Path: r.Path}
	if r.BranchCreated {
		remaining.Branch = r.Branch
	}
	if strings.TrimSpace(r.projectRoot) == "" || strings.TrimSpace(r.Path) == "" {
		return remaining, fmt.Errorf(
			"%w: creation result is incomplete", ErrWorktreeCleanupIncomplete,
		)
	}
	if _, err := os.Stat(r.Path); err != nil {
		if os.IsNotExist(err) {
			return remaining, fmt.Errorf(
				"%w: worktree path disappeared; preserving its branch",
				ErrWorktreeCleanupIncomplete,
			)
		}
		return remaining, fmt.Errorf("%w: inspect worktree path: %v",
			ErrWorktreeCleanupIncomplete, err)
	}
	headOID, headRef, err := lifecycleWorktreeHead(ctx, r.Path)
	if err != nil {
		return remaining, fmt.Errorf("%w: %v", ErrWorktreeCleanupIncomplete, err)
	}
	if !strings.EqualFold(headOID, r.headOID) || headRef != r.headRef {
		return remaining, fmt.Errorf(
			"%w: worktree HEAD changed; preserving it",
			ErrWorktreeCleanupIncomplete,
		)
	}
	branchOID, branchExists, branchErr := lifecycleRefOID(ctx, r.projectRoot, r.Branch)
	if branchErr != nil {
		return remaining, fmt.Errorf("%w: inspect created branch: %v",
			ErrWorktreeCleanupIncomplete, branchErr)
	}
	if !branchExists || !strings.EqualFold(branchOID, r.branchOID) {
		return remaining, fmt.Errorf(
			"%w: created worktree branch changed; preserving it",
			ErrWorktreeCleanupIncomplete,
		)
	}
	dirty, err := worktreeHasRollbackArtifacts(ctx, r.Path)
	if err != nil {
		return remaining, fmt.Errorf("%w: %v", ErrWorktreeCleanupIncomplete, err)
	}
	if dirty {
		return remaining, fmt.Errorf(
			"%w: created worktree contains changes; preserving it",
			ErrWorktreeCleanupIncomplete,
		)
	}
	if out, err := runLifecycleGit(
		ctx, r.projectRoot, "worktree", "remove", "--force", r.Path,
	); err != nil {
		return remaining, fmt.Errorf("remove created worktree: %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	remaining.Path = ""
	if r.BranchCreated {
		if out, err := runLifecycleGit(
			ctx, r.projectRoot, "branch", "-D", "--", r.Branch,
		); err != nil {
			return remaining, fmt.Errorf("delete created branch: %w: %s",
				err, strings.TrimSpace(string(out)))
		}
		remaining.Branch = ""
	}
	return remaining, nil
}

func lifecycleRefOID(ctx context.Context, root, branch string) (string, bool, error) {
	out, err := runLifecycleGit(ctx, root, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)
	if err != nil {
		return "", false, err
	}
	oid := strings.TrimSpace(string(out))
	return oid, oid != "", nil
}

func lifecycleWorktreeHead(
	ctx context.Context, path string,
) (oid string, symbolicRef string, err error) {
	out, err := runLifecycleGit(ctx, path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", "", fmt.Errorf(
			"resolve worktree HEAD: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	oid = strings.TrimSpace(string(out))
	if refOut, refErr := runLifecycleGit(
		ctx, path, "symbolic-ref", "--quiet", "HEAD",
	); refErr == nil {
		symbolicRef = strings.TrimSpace(string(refOut))
	} else if !gitcmd.IsExitCode(refErr, 1) {
		return "", "", fmt.Errorf(
			"resolve worktree symbolic HEAD: %w: %s",
			refErr, strings.TrimSpace(string(refOut)),
		)
	}
	return oid, symbolicRef, nil
}

// RemoveWorktreeOptions parameterizes RemoveWorktreeFromDisk. ProjectRoot
// and Path are required.
type RemoveWorktreeOptions struct {
	ProjectRoot string
	Path        string
	// Branch is the branch the worktree must still have attached and is deleted
	// when RemoveBranch is set. An empty Branch requires a detached worktree and
	// makes RemoveBranch a no-op.
	Branch string
	// Force passes --force to git worktree remove so dirty or locked
	// worktrees still go. Policy checks (refusing dirty removal without
	// force) belong to the caller.
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
// runs the optional teardown hook, removes the worktree (or prunes the
// stale registration when the path is already gone), and optionally
// deletes the branch.
func RemoveWorktreeFromDisk(
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
	hookScript, err := resolveHookScript(root, opts.TeardownScript)
	if err != nil {
		return RemoveWorktreeResult{}, err
	}

	pathExists := true
	if _, statErr := os.Stat(path); statErr != nil {
		if !os.IsNotExist(statErr) {
			return RemoveWorktreeResult{}, fmt.Errorf(
				"stat worktree path: %w", statErr,
			)
		}
		pathExists = false
	}

	result := RemoveWorktreeResult{}
	if hookScript != "" && pathExists {
		if err := verifyRemovalTarget(ctx, root, path, opts.Branch); err != nil {
			return RemoveWorktreeResult{}, err
		}
		if hookErr := runLifecycleHook(
			ctx, hookScript, root, path, opts.Branch, opts.WorktreeName,
			opts.HookEnvironmentPrefix,
		); hookErr != nil {
			return RemoveWorktreeResult{}, hookErr
		}
		result.HookRan = true
		result.HookScript = hookScript
	}

	if pathExists {
		if err := verifyRemovalTarget(ctx, root, path, opts.Branch); err != nil {
			return result, err
		}
		args := []string{"worktree", "remove"}
		if opts.Force {
			// Git requires force twice to remove a locked worktree.
			args = append(args, "--force", "--force")
		}
		args = append(args, path)
		if out, err := runLifecycleGit(ctx, root, args...); err != nil {
			return result, classifyWorktreeGitError(out, err)
		}
	} else {
		if err := verifyRegisteredRemovalTarget(
			ctx, root, path, opts.Branch,
		); err != nil {
			return result, err
		}
		// The directory is gone but git may still hold a stale
		// registration that would block branch deletion and re-creation.
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

// WorktreeIsDirty reports whether the worktree at path has uncommitted
// changes (staged, unstaged, or untracked).
func WorktreeIsDirty(ctx context.Context, path string) (bool, error) {
	out, err := runLifecycleGit(
		ctx, path, "status", "--porcelain", "--untracked-files=all",
	)
	if err != nil {
		return false, fmt.Errorf(
			"check worktree dirty state: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func worktreeHasRollbackArtifacts(ctx context.Context, path string) (bool, error) {
	out, err := runLifecycleGit(
		ctx, path, "status", "--porcelain", "--untracked-files=all",
		"--ignored=matching",
	)
	if err != nil {
		return false, fmt.Errorf(
			"check worktree rollback state: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func verifyRemovalTarget(
	ctx context.Context, root, path, expectedBranch string,
) error {
	rootCommon, err := lifecycleCommonGitDir(ctx, root)
	if err != nil {
		return err
	}
	pathCommon, err := lifecycleCommonGitDir(ctx, path)
	if err != nil {
		return fmt.Errorf("inspect worktree repository: %w", err)
	}
	if rootCommon != pathCommon {
		return fmt.Errorf(
			"worktree path belongs to a different repository: %s", path,
		)
	}
	_, headRef, err := lifecycleWorktreeHead(ctx, path)
	if err != nil {
		return err
	}
	wantRef := ""
	if branch := strings.TrimSpace(expectedBranch); branch != "" {
		wantRef = "refs/heads/" + branch
	}
	if headRef != wantRef {
		return fmt.Errorf(
			"worktree branch changed: expected %q, found %q",
			wantRef, headRef,
		)
	}
	return nil
}

func verifyRegisteredRemovalTarget(
	ctx context.Context, root, path, expectedBranch string,
) error {
	out, err := runLifecycleGit(
		ctx, root, "worktree", "list", "--porcelain",
	)
	if err != nil {
		return fmt.Errorf(
			"list registered worktrees: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	wantPath := comparableWorktreePath(path)
	wantBranch := strings.TrimSpace(expectedBranch)
	for _, entry := range gitworktree.ParsePorcelain(string(out)) {
		if comparableWorktreePath(entry.Path) != wantPath {
			continue
		}
		if entry.Branch != wantBranch ||
			(wantBranch == "" && !entry.Detached) {
			return fmt.Errorf(
				"stale worktree registration branch changed: expected %q, found %q",
				wantBranch, entry.Branch,
			)
		}
		return nil
	}
	return fmt.Errorf("worktree registration not found: %s", path)
}

func comparableWorktreePath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = filepath.Clean(path)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	} else if parent, parentErr := filepath.EvalSymlinks(
		filepath.Dir(absolute),
	); parentErr == nil {
		absolute = filepath.Join(parent, filepath.Base(absolute))
	}
	absolute = filepath.Clean(absolute)
	if filepath.Separator == '\\' {
		absolute = strings.ToLower(absolute)
	}
	return absolute
}

func lifecycleCommonGitDir(ctx context.Context, path string) (string, error) {
	out, err := runLifecycleGit(ctx, path, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf(
			"resolve common Git directory: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	common := strings.TrimSpace(string(out))
	if !filepath.IsAbs(common) {
		common = filepath.Join(path, common)
	}
	common, err = filepath.Abs(common)
	if err != nil {
		return "", fmt.Errorf("resolve common Git directory path: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(common); resolveErr == nil {
		common = resolved
	}
	return filepath.Clean(common), nil
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

// resolveHookScript resolves a caller-supplied hook script path against the
// project root and rejects paths that escape it. Both sides of the
// containment check are canonicalized through symlink resolution so a
// symlink inside the project cannot smuggle in a script that lives outside
// it. An empty raw path means no hook and resolves to "".
func resolveHookScript(projectRoot, raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	resolved := trimmed
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(projectRoot, resolved)
	}
	resolved = filepath.Clean(resolved)
	if !pathWithinRoot(canonicalizePath(projectRoot), canonicalizePath(resolved)) {
		return "", fmt.Errorf("%w: %q", ErrHookOutsideProject, raw)
	}
	return resolved, nil
}

// canonicalizePath resolves symlinks when the path exists; a path that does
// not exist yet (or cannot be resolved) keeps its lexical form, which fails
// later at execution time rather than here.
func canonicalizePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
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
		if base == "" {
			base = root + "-worktrees"
		}
		if err := os.MkdirAll(base, 0o755); err != nil {
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
	if runner.Env == nil {
		isZero := len(runner.Config) == 0 &&
			!runner.StripEnv &&
			!runner.TerminalPrompt &&
			!runner.NullGlobalConfig &&
			!runner.NoSystemConfig &&
			!runner.DisableSafeDirectoryForward
		runner.Env = os.Environ()
		if isZero {
			runner.StripEnv = true
		}
	}
	return context.WithValue(ctx, lifecycleExecutionContextKey{}, lifecycleExecution{
		runner: runner, runGit: runGit, runHook: runHook,
	})
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
	script, projectRoot, worktreePath, branch, worktreeName,
	environmentPrefix string,
) error {
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
		Script: script, Dir: worktreePath, Env: environment,
		Stdout: io.Discard, Stderr: &stderr,
	}
	var err error
	if run := lifecycleHookRunner(ctx); run != nil {
		err = run(ctx, command)
	} else {
		cmd := exec.CommandContext(ctx, command.Script)
		cmd.Dir = command.Dir
		cmd.Env = command.Env
		cmd.Stdout = command.Stdout
		cmd.Stderr = command.Stderr
		err = cmd.Run()
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("run lifecycle hook %s: %w", script, ctxErr)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &HookError{
				Script:   script,
				ExitCode: exitErr.ExitCode(),
				Stderr:   strings.TrimSpace(stderr.String()),
			}
		}
		return fmt.Errorf("run lifecycle hook %s: %w", script, err)
	}
	return nil
}

// rollbackCreatedWorktree best-effort unwinds a worktree this call just
// created after its setup hook failed. The branch is deleted only when
// this call created it; the original hook error is what the caller
// surfaces.
func rollbackCreatedWorktree(
	ctx context.Context, root, path, branch string, deleteBranch bool,
) {
	_, _ = rollbackCreatedWorktreeWithResult(ctx, root, path, branch, deleteBranch)
}

func rollbackCreatedWorktreeWithResult(
	ctx context.Context, root, path, branch string, deleteBranch bool,
) (RollbackResult, error) {
	var result RollbackResult
	var errs []error
	if out, err := runLifecycleGit(
		ctx, root, "worktree", "remove", "--force", path,
	); err != nil {
		result.Path = path
		errs = append(errs, fmt.Errorf("remove created worktree: %w: %s", err, strings.TrimSpace(string(out))))
	}
	if deleteBranch {
		if out, err := runLifecycleGit(ctx, root, "branch", "-D", "--", branch); err != nil {
			result.Branch = branch
			errs = append(errs, fmt.Errorf("delete created branch: %w: %s", err, strings.TrimSpace(string(out))))
		}
	}
	return result, errors.Join(errs...)
}
