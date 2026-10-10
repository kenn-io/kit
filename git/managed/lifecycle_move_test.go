package managedworktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func TestMoveWorktreeReturnsRollbackEvidence(t *testing.T) {
	for _, branch := range []string{"", "topic"} {
		t.Run("branch="+branch, func(t *testing.T) {
			root := initLifecycleRepo(t)
			source := filepath.Join(t.TempDir(), "prepared")
			destination := filepath.Join(t.TempDir(), "workspace")
			args := []string{"worktree", "add", "--detach", source, "HEAD"}
			if branch != "" {
				args = []string{"worktree", "add", "-b", branch, source, "HEAD"}
			}
			lifecycleGit(t, root, args...)
			require.NoError(t, os.WriteFile(filepath.Join(source, "retained"), []byte("prepared content"), 0o600))
			before, err := os.Stat(filepath.Join(source, "retained"))
			require.NoError(t, err)
			// Windows loads file identity lazily; capture it before the old path moves.
			require.True(t, os.SameFile(before, before))
			result, err := MoveWorktreeOnDisk(t.Context(), MoveWorktreeOptions{ProjectRoot: root, Source: source, Path: destination, Runner: lifecycleTestRunner(t)})
			require.NoError(t, err)
			require.Equal(t, destination, result.Path)
			require.Equal(t, branch, result.Branch)
			require.False(t, result.BranchCreated)
			after, err := os.Stat(filepath.Join(destination, "retained"))
			require.NoError(t, err)
			require.True(t, os.SameFile(before, after))
			_, err = result.Rollback(t.Context(), RollbackUnchanged)
			require.ErrorIs(t, err, ErrWorktreeCleanupIncomplete)
			require.NoError(t, os.Remove(filepath.Join(destination, "retained")))
			_, err = result.Rollback(t.Context(), RollbackUnchanged)
			require.NoError(t, err)
			require.NoDirExists(t, destination)
			if branch != "" {
				require.True(t, branchExistsInRepo(t, root, branch))
			}
		})
	}
}

func TestMoveWorktreeCapturesSuccessfulMoveAfterCancellation(t *testing.T) {
	root := initLifecycleRepo(t)
	source := filepath.Join(t.TempDir(), "prepared")
	destination := filepath.Join(t.TempDir(), "workspace")
	lifecycleGit(t, root, "worktree", "add", "--detach", source, "HEAD")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result, err := MoveWorktreeOnDisk(ctx, MoveWorktreeOptions{ProjectRoot: root, Source: source, Path: destination, Runner: lifecycleTestRunner(t), RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
		out, err := runner.Output(ctx, dir, args...)
		if err == nil && strings.Join(args, " ") == "worktree move -- "+source+" "+destination {
			cancel()
			return out, nil
		}
		return out, err
	}})
	require.ErrorIs(t, err, context.Canceled)
	require.DirExists(t, destination)
	_, err = result.Rollback(t.Context(), RollbackUnchanged)
	require.NoError(t, err)
	require.NoDirExists(t, destination)
}

func TestMoveWorktreeRejectsForeignPrimaryAndOccupiedTargets(t *testing.T) {
	root := initLifecycleRepo(t)
	source := filepath.Join(t.TempDir(), "prepared")
	destination := filepath.Join(t.TempDir(), "workspace")
	lifecycleGit(t, root, "worktree", "add", "--detach", source, "HEAD")
	for _, name := range []string{"primary", "foreign", "occupied", "failed move"} {
		t.Run(name, func(t *testing.T) {
			opts := MoveWorktreeOptions{ProjectRoot: root, Source: source, Path: destination, Runner: lifecycleTestRunner(t)}
			switch name {
			case "primary":
				opts.Source = root
			case "foreign":
				opts.ProjectRoot = initLifecycleRepo(t)
			case "occupied":
				require.NoError(t, os.Mkdir(destination, 0o700))
				t.Cleanup(func() { require.NoError(t, os.Remove(destination)) })
			case "failed move":
				opts.RunGit = func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
					if len(args) > 1 && args[0] == "worktree" && args[1] == "move" {
						return nil, errors.New("move unavailable")
					}
					return r.Output(ctx, dir, args...)
				}
			}
			result, err := MoveWorktreeOnDisk(t.Context(), opts)
			require.Error(t, err)
			_, err = result.Rollback(t.Context(), RollbackFreshOwned)
			require.ErrorIs(t, err, ErrWorktreeCleanupIncomplete)
			require.DirExists(t, source)
			require.DirExists(t, root)
		})
	}
}

func TestMoveWorktreeUnknownOutcomePreservesDestination(t *testing.T) {
	root := initLifecycleRepo(t)
	source := filepath.Join(t.TempDir(), "prepared")
	destination := filepath.Join(t.TempDir(), "workspace")
	lifecycleGit(t, root, "worktree", "add", "--detach", source, "HEAD")
	failure := errors.New("unknown process outcome")
	result, err := MoveWorktreeOnDisk(t.Context(), MoveWorktreeOptions{ProjectRoot: root, Source: source, Path: destination, Runner: lifecycleTestRunner(t), RunGit: func(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
		out, err := r.Output(ctx, dir, args...)
		if err == nil && len(args) > 1 && args[0] == "worktree" && args[1] == "move" {
			return out, failure
		}
		return out, err
	}})
	require.ErrorIs(t, err, failure)
	require.Equal(t, destination, result.Path)
	remaining, err := result.Rollback(t.Context(), RollbackFreshOwned)
	require.ErrorIs(t, err, ErrWorktreeCleanupIncomplete)
	require.True(t, remaining.Unverified)
	require.Equal(t, destination, remaining.Path)
	require.DirExists(t, destination)
}

func TestMovePreparedWorktreeCreatesOwnedBranch(t *testing.T) {
	for _, outcome := range []string{"success", "move failure", "unknown move", "branch exists"} {
		t.Run(outcome, func(t *testing.T) {
			root := initLifecycleRepo(t)
			source := filepath.Join(t.TempDir(), "prepared")
			destination := filepath.Join(t.TempDir(), "workspace")
			lifecycleGit(t, root, "worktree", "add", "--detach", source, "HEAD")
			before, err := os.Stat(source)
			require.NoError(t, err)
			require.True(t, os.SameFile(before, before))
			failure := errors.New("move interrupted")
			if outcome == "branch exists" {
				lifecycleGit(t, root, "branch", "topic")
			}
			result, err := MoveWorktreeOnDisk(t.Context(), MoveWorktreeOptions{
				ProjectRoot: root, Source: source, Path: destination, NewBranch: "topic", BaseRef: "HEAD", Runner: lifecycleTestRunner(t),
				RunGit: func(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
					moving := len(args) > 1 && args[0] == "worktree" && args[1] == "move"
					if moving && outcome == "move failure" {
						return nil, failure
					}
					out, err := runner.Output(ctx, dir, args...)
					if moving && err == nil && outcome == "unknown move" {
						return out, failure
					}
					return out, err
				},
			})
			if outcome == "branch exists" {
				require.ErrorIs(t, err, ErrBranchAlreadyExists)
				require.DirExists(t, source)
				require.Empty(t, result.Path)
				return
			}
			if outcome == "success" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, failure)
			}
			require.True(t, result.BranchCreated)
			path := destination
			if outcome == "move failure" {
				path = source
			}
			after, err := os.Stat(path)
			require.NoError(t, err)
			require.True(t, os.SameFile(before, after))
			remaining, err := result.Rollback(t.Context(), RollbackUnchanged)
			if outcome == "unknown move" {
				require.ErrorIs(t, err, ErrWorktreeCleanupIncomplete)
				require.True(t, remaining.Unverified)
				require.DirExists(t, path)
				require.True(t, branchExistsInRepo(t, root, "topic"))
			} else {
				require.NoError(t, err)
				require.NoDirExists(t, path)
				require.False(t, branchExistsInRepo(t, root, "topic"))
			}
		})
	}
}
