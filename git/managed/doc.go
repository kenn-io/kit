// Package managedworktree provides the named worktree lifecycle used by
// interactive developer tools.
//
// It creates or attaches branch-backed worktrees, imports pull or merge request
// heads with push tracking, runs optional lifecycle hooks, detects dirty state,
// and removes worktrees and branches with classified errors.
package managedworktree
