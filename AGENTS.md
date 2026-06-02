# Agent Instructions

## Project Overview

`go.kenn.io/kit` is a Go module of reusable building blocks for Kenn CLI and
developer tools. Keep packages small, app-neutral, and usable by more than one
caller. Do not add product-specific config, database state, UI behavior, or
provider workflows to this repo unless the package already owns that concern.

## How To Work Here

- Start from the package intent, not from a calling app's current needs. The
  reusable package should stay useful to future callers with similar mechanics.
- Prefer the repo's existing helpers and test fixtures over recreating local
  variants in a caller.
- Follow the Go version and dependency choices in `go.mod`; do not pin guidance
  here to a specific version unless the code requires that version for a reason.
- Keep Unix and Windows behavior explicit when permissions, ownership, sockets,
  paths, or process behavior differ.
- When behavior changes, update the package-level `AGENTS.md` nearest the change
  if it captures an invariant future agents need to preserve.
- Let CI, `go.mod`, and tool config files define the exact verification
  commands. Do not duplicate command recipes here unless the command carries a
  repo-specific intent that is not encoded elsewhere.

## Go Conventions

- Use standard library APIs first and add dependencies only when they pay for
  themselves.
- Keep public package APIs narrow and app-neutral. Callers should not need to
  import unrelated kit packages to use one package correctly.
- Surface errors with enough context for callers to act on them. Do not turn
  failures into success-shaped fallbacks.
- Use `context.Context` for subprocesses, network calls, update checks, probes,
  and other work that can block.
- Avoid broad cleanup or mutation. Operate on exact paths, runtime records, and
  repositories that the caller supplied or the test created.

## Tests

- Use `github.com/stretchr/testify` for new and changed tests. Prefer
  `require` for setup, preconditions, and values used later; use `assert` for
  independent checks where more failures help diagnosis.
- Do not add new `t.Fatal`, `t.Fatalf`, `t.Error`, `t.Errorf`, `t.Fail`, or
  `t.FailNow` calls. Existing tests still contain some stdlib assertions; when
  editing those checks, migrate the touched checks to testify if it keeps the
  test readable.
- When a test repeats package-level testify calls, create local helpers such as
  `assert := assert.New(t)` or `require := require.New(t)` and use the helper
  methods for the repeated checks.
- Prefer table tests when they make input and expected behavior clearer.
- Use `t.TempDir()` for files created by tests unless the test specifically
  needs a fixed OS temp path to exercise permissions or runtime-dir behavior.
- Tests must not depend on the user's git config, global credentials, real
  repositories, home directory state, or live provider availability.

## Git Workflow

- Do not change branches unless the user explicitly asks.
- Do not amend commits unless the user explicitly asks.
- Never revert user changes. If existing edits touch the same files, read them
  and work with them.
