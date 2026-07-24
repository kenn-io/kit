# Git Package Instructions

## Scope

The `git/` tree provides reusable helpers for developer tools that inspect and
mutate Git repositories. Keep these packages about Git mechanics rather than a
specific application or forge workflow.

## Package Rules

- Prefer `gitcmd.New()` for Git subprocesses so callers get consistent
  environment and prompt handling.
- Do not call `exec.Command("git", ...)` directly in package code unless the
  direct call is the behavior being tested.
- Pass `context.Context` through Git operations that can block.
- Return Git failures with captured stderr. Do not hide Git's message behind a
  generic error.
- Keep remote and clone-path parsing in `gitremote`; do not duplicate it in
  sibling packages.
- Do not assume GitHub-only identity. Keep host, owner, repository name, and
  provider-specific merge-request refs explicit.
- `git/managed` owns the shared named-worktree lifecycle. Extend it instead of
  creating application-local worktree creation, merge-request import,
  tracking, hook, or rollback implementations.
- The managed lifecycle trusts the existing repository, remotes, Git
  configuration, provider metadata, lifecycle hooks, and same-user filesystem
  state. This includes configured remote push URLs and refspecs. Ordinary
  named-worktree creation also trusts the checked-out tree.
- Merge-request import treats the fetched tree as untrusted: keep checkout-time
  hooks disabled, neutralize configured filter/fsmonitor/diff/merge programs
  the tree can select, disable implicit submodule recursion, and persist those
  settings in the imported worktree. This is a narrow Git execution boundary,
  not an OS sandbox for hostile configuration, remotes, lifecycle scripts,
  same-user replacement races, or resource exhaustion.
- Reject isolation-sensitive command-scope configuration during import because
  worktree configuration cannot outrank it. Explicit command-scope overrides
  on later Git commands are caller policy, not a sandbox boundary Kit can
  enforce.
- The default lifecycle-hook runner is for trusted native executables. Callers
  that need process-tree supervision or cross-platform script dispatch must
  supply `RunHook`; do not grow those application policies into this package.
- Worktree base permissions follow the caller's path and host umask. Owner-only
  directory policy and platform ACL management belong to the application.
- Expected merge-request head SHAs are correctness anchors: verify them before
  creating the local branch or materializing a worktree.
- Rollback after a completed create is conservative about ordinary user work:
  preserve a dirty worktree, an initialized submodule, or an advanced branch
  and report `ErrWorktreeCleanupIncomplete`. Cleanup performed immediately
  after an in-operation failure may force-remove artifacts created by that
  operation.
- Configure merge-request tracking in worktree-scoped Git configuration so
  removing a worktree does not leave branch routing behind in shared config.
- Lifecycle hooks must resolve inside the project tree. Applications may
  supply Git and hook runners to retain their process limits and
  platform-specific execution policy.

## Tests

- Use `git/test` fixtures or temporary local repositories instead of the
  user's repositories.
- Tests must not read or mutate global Git config. Set needed identity and
  configuration inside the fixture.
- Prefer testify assertions for new and changed checks.
