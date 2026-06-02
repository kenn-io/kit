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
- Return git failures with captured stderr. Do not hide git's message behind a
  generic error.
- Use `gitlock` around mutations that can race with another process touching
  the same repository.
- Keep remote and clone-path validation in `gitremote`; do not duplicate that
  parsing in callers or sibling packages.
- Do not assume GitHub-only identity. Keep host, owner, and repository name
  handling explicit.

## Tests

- Use `git/test` fixtures instead of the user's real repositories.
- Tests must not read or mutate global git config. Set needed identity/config
  inside the fixture.
- Use temporary directories and clean them with test cleanup hooks.
- Prefer testify assertions for new or changed checks.
