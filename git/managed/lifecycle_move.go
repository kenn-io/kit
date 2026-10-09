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
	Runner                    gitcmd.Runner
	RunGit                    GitRunner
}

// MoveWorktreeOnDisk transfers an existing checkout to Path and returns evidence
// for explicit rollback of that acquired destination. Rollback removes the moved
// checkout, not its preexisting branch; it does not move the checkout back.
// No automatic cleanup runs on error. A failed or unknown Git exit grants no
// cleanup authority, even if Git moved some artifacts before reporting failure.
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
	_, moveErr := runLifecycleGit(ctx, root, "worktree", "move", "--", source, path)
	if moveErr != nil && !gitcmd.IsExitCode(moveErr, 0) {
		return failedWorktreeAdd(ctx, root, path, branch, false), moveErr
	}
	captureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	result, captureErr := snapshotCreateWorktreeResult(captureCtx, root, path, branch, false)
	return result, errors.Join(moveErr, ctx.Err(), captureErr)
}
