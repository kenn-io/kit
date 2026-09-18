# Adopting the shared Go lint policy (kennlint)

`go.kenn.io/kit/lint` owns the Go lint policy for kenn-io repositories: a
canonical golangci-lint configuration, five custom analyzers, and a
golangci-lint module plugin that runs them. This document explains how to put a
repository on the shared policy and how to keep it there.

The policy assumes the Go language version in kit's `go.mod` or newer. Some
rules point at APIs that older toolchains lack: the `errors.As` ban expects
`errors.AsType` (Go 1.26), `usetesting` expects `t.Context()` (Go 1.24), and
`sleeptest` expects `synctest.Test` (Go 1.25). A repository on an older
language version should raise it before adopting, or disable those rules in
its overlay until it can.

## What you get

**One configuration.** golangci-lint has no configuration inheritance, so each
repository used to copy a config and drift. Instead a repository commits only
an overlay with its local additions, and `kennlint config` renders the merged
`.golangci.yml` from the canonical file plus the overlay. A `-check` mode fails
CI when the committed file is stale.

**Five analyzers** that off-the-shelf linters do not cover. They ship as the
`kennlint` linter inside a custom golangci-lint build, so `//nolint:kennlint`
and path exclusions work like any other linter.

| Analyzer | Reports |
| --- | --- |
| `sqlclosecheck`, `rowserrcheck` | SQL resource closure and iteration errors, including returned ownership and shared scanners across packages. The kit versions extend the upstream checks; do not enable the duplicate built-in checks. |
| `testifyhelper` | Tests that repeat package-level `assert.X(t, …)`/`require.X(t, …)` calls instead of creating `assert := assert.New(t)` or `require := require.New(t)` once. Any variable bound to `New(t)` counts, so a test whose subtests need their own helper can name the outer one `req` or `asrt` to avoid shadowing the package. |
| `sleeptest` | `time.Sleep` in a `_test.go` file outside a `synctest.Test` bubble. Wall-clock sleeps make tests slow and timing-dependent. A bubble is the body of the function passed to `synctest.Test`, inline or by name, at any nesting depth including goroutines. A helper that sleeps and is only called from inside a bubble is still reported: give it a channel to wait on instead. Packages named `testutil` or ending in `test` are checked too (`helper-packages`, default on). Set `eventually: true` to also report testify `Eventually`, `EventuallyWithT`, and `Never` outside bubbles; that is off by default because some repositories endorse `Eventually` for awaiting a fake's channel. |
| `errtext` | Deciding on error identity by matching `err.Error()` text: `strings.Contains(err.Error(), …)`, `err.Error() == …`, and similar. Use `errors.Is` or `errors.AsType`. Test files are skipped unless `errtext.include-tests` is set. |
| `sqlcheck` | SQL `CHECK` constraints and `CREATE TYPE ... AS ENUM` in Go string literals outside tests. A CHECK locks a validation rule into the schema, so every change to the rule (most often a new allowed value for a status-like column) needs a migration that rewrites the constraint. Validate in application code or keep allowed values in a lookup table. `kennlint sql` applies the same check to `.sql` migration files. |
| `nohttpmux` | `Handle`/`HandleFunc` on `*http.ServeMux` or the default mux outside tests, for repositories that route every operation through a typed API layer such as Huma. Disable it in repositories that serve plain `net/http`. |

`kennlint analyzers` prints this list from the binary.

## Adopting a repository

1. Add the plugin to `.custom-gcl.yml` (create the file if the repository does
   not already build a custom golangci-lint):

   ```yaml
   version: v2.13.1
   plugins:
     - module: "go.kenn.io/kit"
       import: "go.kenn.io/kit/lint/gclplugin"
       version: "<kit version from go.mod>"
   ```

   The kit version should match the one already required by `go.mod` so the
   custom build resolves the same module graph. Repositories that already use
   the nilaway plugin keep it in the same list.

2. Write the overlay, `.golangci.overlay.yml`, with only the repository's own
   additions: `importas` aliases, extra `forbidigo` patterns, exclusion rules
   for known carve-outs. Leave it out entirely if there are none.

3. Render and commit the configuration:

   ```sh
   go run go.kenn.io/kit/cmd/kennlint@<kit version> config
   ```

4. Build the custom binary and run it:

   ```sh
   go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1 custom \
     --destination . --name custom-gcl --version v2.13.1
   ./custom-gcl run ./...
   ```

   Add `custom-gcl` to `.gitignore`. golangci-lint caches the build and skips it
   when the plugin list is unchanged.

5. Wire the two commands into the repository's lint target and pre-commit
   hooks, and add a drift check to CI:

   ```sh
   go run go.kenn.io/kit/cmd/kennlint@<kit version> config -check
   ```

   The kit repository's `Makefile` and `prek.toml` are a working example.

## Overlay semantics

`kennlint config` merges the overlay into the canonical configuration
recursively:

- Mapping keys present in both are merged; keys only in the overlay are added.
- Lists are appended, skipping items the canonical list already contains. This
  is how `linters.enable`, `forbidigo.forbid`, `importas.alias`, and
  `exclusions.rules` grow per repository.
- Scalars in the overlay replace the canonical value, for example
  `run.timeout`.
- `linters.disable` and `formatters.disable` in the overlay remove those names
  from the rendered `enable` lists and are not emitted themselves. Use them for
  a documented backlog, not as a permanent opt-out.

Plugin settings live under `linters.settings.custom.kennlint.settings`:

```yaml
linters:
  settings:
    custom:
      kennlint:
        settings:
          disable: [nohttpmux]      # analyzers to leave out
          errtext:
            include-tests: true     # also report err.Error() matching in tests
```

## Rolling out on a repository with a backlog

Turning the full policy on a large repository usually surfaces hundreds of
existing findings. Two staged approaches work:

- **Backlog overlay.** Disable the linters with large counts in the overlay,
  file an issue listing them, and remove entries as the backlog burns down.
  This keeps CI independent of git history.
- **Only new code.** Set `issues.new-from-merge-base: origin/main` in the
  overlay so golangci-lint reports only findings introduced on the branch.
  This needs full history in CI checkouts.

Either way, suppress individual findings with `//nolint:<linter> // reason`.
Two rules that trip existing tests: `t.Context()` is already canceled inside
`t.Cleanup`, so derive `context.WithoutCancel(t.Context())` there, and helper
subprocesses started with `exec.CommandContext(t.Context(), …)` are killed at
cleanup unless they use the same uncancelled context.
The canonical `nolintlint` settings require the linter name and an explanation
and reject directives that no longer suppress anything.

## Checking SQL migration files

golangci-lint only sees Go, so migrations kept as `.sql` files need the
separate scanner:

```sh
go run go.kenn.io/kit/cmd/kennlint@<kit version> sql internal/db/migrations
```

It prints `file:line:column: message` for each `CHECK` constraint and each
`CREATE TYPE ... AS ENUM`, and exits non-zero when it finds any. Wire it into
the lint target or a pre-commit hook filtered to `*.sql`.

Every `CHECK` is reported, not only ones that spell out a value list. Range
and length checks and cross-column invariants lock a rule into the schema in
exactly the same way, and in practice most of them exist to police a set of
states. `CHECK` text inside SQL comments and string literals is ignored, so a
migration that removes a constraint can mention it in a comment. Where a set
really must live in the database, put it in a lookup table with a foreign
key.

## Running the analyzers without golangci-lint

golangci-lint cannot load an analyzer from an imported package at runtime;
its only extension points are the module plugin used here, which needs the
`golangci-lint custom` build, and Go `.so` plugins. Repositories that already
build a custom binary for nilaway pay nothing extra for the plugin.

Without a custom binary, `kennlint run ./...` runs the five analyzers directly
through the standard `go/analysis` multichecker, with the usual `-json`,
`-fix`, and per-analyzer flags such as `-errtext.include-tests` or
`-sleeptest.eventually`; `go vet -vettool=$(command -v kennlint) ./...` works
too. Both are useful for editors and for repositories that cannot build a
custom golangci-lint, but neither honors `nolint` comments or golangci path
exclusions. Rolling `sleeptest` out to a repository with existing sleeps
works best through golangci-lint's `new-from-rev` so new sleeps are blocked
while the backlog is burned down.

## Changing the policy

The canonical configuration is `lint/config/golangci.yml`. Analyzers live in
`lint/<name>/` with `analysistest` fixtures under `testdata/`. Changes there
land in every repository on its next kit upgrade and `kennlint config` run, so
prefer additive changes and describe the rationale in the pull request.
