# Git Package Instructions

## Scope

The `git/` tree provides reusable helpers for automation that needs to inspect,
mutate, or isolate git repositories. Keep these packages about git mechanics,
not about one product's workflow.

## Package Rules

- Prefer `gitcmd.New()` for git subprocesses so automation stays insulated from
  inherited repository state, global config, prompts, and credential leaks.
- Do not call `exec.Command("git", ...)` directly in package code unless the
  direct call is the behavior being tested.
- Pass `context.Context` through git operations that can block.
- Keep best-effort helper probes bounded; they must not stall the caller's real
  git command when optional ambient state is slow or broken.
- Return git failures with captured stderr. Do not hide git's message behind a
  generic error.
- Use `gitlock` around mutations that can race with another process touching
  the same repository.
- Keep remote and clone-path validation in `gitremote`; do not duplicate that
  parsing in callers or sibling packages.
- Do not assume GitHub-only identity. Keep host, owner, and repository name
  handling explicit.
- `git/managed` is the shared named-worktree lifecycle. Extend that package
  instead of creating app-local worktree creation, merge-request import,
  tracking, hook, or rollback implementations.
- Keep managed worktree rollback ownership-conservative: path identity, branch
  OID, symbolic HEAD, and worktree HEAD OID must still match their creation
  snapshots before destructive cleanup.
- Default managed worktree bases must stay restricted to the current user on
  Unix and Windows. Callers opt into shared permissions with an explicit base.
- Lifecycle hook cancellation must terminate the spawned process tree, not
  only the immediate hook process.

## Tests

- Use `git/test` fixtures instead of the user's real repositories.
- Tests must not read or mutate global git config. Set needed identity/config
  inside the fixture.
- Use temporary directories and clean them with test cleanup hooks.
- Prefer testify assertions for new or changed checks.
