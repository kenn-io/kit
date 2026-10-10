package managedworktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	gitcmd "go.kenn.io/kit/git/cmd"
)

// MoveWorktreeOptions relocates an existing linked checkout. Path must be absent;
// Git updates its registration and preserves its files, index, and branch.
type MoveWorktreeOptions struct {
	ProjectRoot, Source, Path string
	// NewBranch creates a branch in a detached prepared checkout before moving it.
	// BaseRef selects its commit and is valid only with NewBranch.
	NewBranch, BaseRef string
	Runner             gitcmd.Runner
	RunGit             GitRunner
}

// MoveWorktreeOnDisk transfers an existing checkout to Path and returns evidence
// for explicit rollback of that acquired destination. Rollback removes the moved
// checkout and any unchanged branch created by this call; it does not move the
// checkout back. Existing branches remain intact.
// No automatic cleanup runs on error. A failed or unknown Git exit grants no
// cleanup authority for the destination, even if Git moved some artifacts before
// reporting failure. A confirmed new-branch checkout may still be rolled back at
// Source when the move failed and that acquisition remains there.
func MoveWorktreeOnDisk(ctx context.Context, opts MoveWorktreeOptions) (CreateWorktreeResult, error) {
	var empty CreateWorktreeResult
	root, err := absRequired(opts.ProjectRoot, "project root")
	if err != nil {
		return empty, err
	}
	source, err := absRequired(opts.Source, "source worktree")
	if err != nil {
		return empty, err
	}
	path, err := absRequired(opts.Path, "worktree destination")
	if err != nil {
		return empty, err
	}
	ctx = withLifecycleExecution(ctx, opts.Runner, opts.RunGit, nil)
	info, err := os.Lstat(source)
	if err != nil {
		return empty, err
	}
	if !info.IsDir() {
		return empty, fmt.Errorf("source worktree must be a directory: %w", ErrInvalidWorktreeOptions)
	}
	if _, err = os.Lstat(path); err == nil {
		return empty, ErrWorktreeDestinationExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return empty, err
	}
	_, headRef, err := lifecycleWorktreeHead(ctx, source)
	if err != nil {
		return empty, err
	}
	branch := strings.TrimPrefix(headRef, "refs/heads/")
	if err = verifyRemovalTarget(ctx, root, source, branch); err != nil {
		return empty, err
	}
	var prepared CreateWorktreeResult
	if opts.NewBranch == "" && opts.BaseRef != "" {
		return empty, ErrInvalidWorktreeOptions
	}
	if opts.NewBranch != "" {
		if branch != "" {
			return empty, fmt.Errorf("prepared source must be detached: %w", ErrInvalidWorktreeOptions)
		}
		branch = strings.TrimSpace(opts.NewBranch)
		if err := validateBranchName(ctx, root, branch); err != nil {
			return empty, err
		}
		exists, err := localBranchExists(ctx, root, branch)
		if err != nil {
			return empty, err
		}
		if exists {
			return empty, fmt.Errorf("%w: %s", ErrBranchAlreadyExists, branch)
		}
		ref := opts.BaseRef
		if ref == "" {
			ref = "HEAD"
		}
		oid, err := resolveMergeRequestOID(ctx, root, ref)
		if err != nil {
			return empty, err
		}
		out, checkoutErr := runLifecycleGit(ctx, source, "-c", "submodule.recurse=false", "checkout", "--no-guess", "--no-track", "-b", branch, oid)
		if checkoutErr != nil && !gitcmd.IsExitCode(checkoutErr, 0) {
			return failedWorktreeAdd(ctx, root, source, branch, true), classifyWorktreeGitError(out, checkoutErr)
		}
		captureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		prepared, err = snapshotCreateWorktreeResult(captureCtx, root, source, branch, true)
		cancel()
		prepared.headOID, prepared.branchOID, prepared.headRef = oid, oid, "refs/heads/"+branch
		if cause := errors.Join(checkoutErr, ctx.Err(), err); cause != nil {
			return prepared, cause
		}
	}
	_, moveErr := runLifecycleGit(ctx, root, "worktree", "move", "--", source, path)
	if moveErr != nil && !gitcmd.IsExitCode(moveErr, 0) {
		if opts.NewBranch != "" {
			inspectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if prepared.verifyAcquisition(inspectCtx) == nil {
				return prepared, moveErr
			}
		}
		return failedWorktreeAdd(ctx, root, path, branch, opts.NewBranch != ""), moveErr
	}
	captureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	result, captureErr := snapshotCreateWorktreeResult(captureCtx, root, path, branch, opts.NewBranch != "")
	if opts.NewBranch != "" {
		result.headOID, result.branchOID, result.headRef = prepared.headOID, prepared.branchOID, prepared.headRef
	}
	return result, errors.Join(moveErr, ctx.Err(), captureErr)
}
