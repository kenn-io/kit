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
  snapshots before destructive cleanup. If a complete creation snapshot cannot
  be obtained, preserve the artifacts and report incomplete cleanup. Created
  branches must remain direct refs and be deleted with no-dereference semantics.
- Default managed worktree bases must stay restricted to the current user on
  Unix and Windows. Callers opt into shared permissions with an explicit base.
- Lifecycle hooks must resolve to an existing regular file inside the project
  before worktree mutation and retain the same target and file identity at
  execution time. Hook-isolation directories must be private to the current
  user on Unix and Windows.
- Conservative rollback must include ignored and untracked artifacts when it
  decides whether a managed worktree still matches its creation snapshot.
- Interactive Git commands must retain foreground terminal access; automated
  Git and lifecycle hooks must keep bounded process-tree cancellation, and hook
  cancellation must terminate the spawned tree rather than only its parent.
- Contributor-controlled checkout requires Git 2.39.1 or newer on every
  platform. Windows additionally requires Git for Windows
  2.53.0.windows.3 or newer; keep the full Windows patch level in version
  validation because a base `2.53.0` version is not sufficient. Enforce this
  in the public isolated-checkout path before worktree mutation, not only in
  change-request orchestration.
- When an expected head OID is the project-remote provenance anchor, every fetch
  through that boundary must enforce the stored OID before publishing a
  destination ref. Publish merge-request heads only under a request-numbered
  Kit namespace, reject symbolic or case-alias destinations, and terminate Git
  fetch options before remote/ref arguments. Fetch only through remote names
  previously validated by the managed boundary, revalidate their effective URLs
  immediately before use, and stage fetched OIDs in operation-private Kit refs
  rather than the repository-wide `FETCH_HEAD`. Windows lifecycle scripts must
  honor their declared shebang interpreter regardless of file extension and
  preserve native absolute interpreter paths. Private hook snapshots must keep
  owner-write permission on Windows so successful execution does not leak a
  read-only temporary file.
- Before teardown hooks or destructive removal, verify that the path is still
  the registered worktree for the expected repository and branch. Preserve
  replacement paths and repositories, require detached worktrees to remain
  detached, and report incomplete cleanup when ownership changed. Dirty-state
  checks must explicitly include untracked files regardless of repository
  status configuration.
- Windows Job Objects may kill a Git process tree on cancellation or failure,
  but successful Git commands—including roots that return `exec.ErrWaitDelay`
  because a descendant retained output handles—must preserve those descendants.
- Keep transport classification platform-aware: drive-letter syntax is local
  only on Windows, while identity parsers may recognize that syntax on any host.
- Canonicalize relative local clone paths against the project root before
  deriving repository identity or persisting a remote. Configure change-request
  tracking only when the provider supplied an explicit source branch.

## Tests

- Use `git/test` fixtures instead of the user's real repositories.
- Tests must not read or mutate global git config. Set needed identity/config
  inside the fixture.
- Use temporary directories and clean them with test cleanup hooks.
- Prefer testify assertions for new or changed checks.
