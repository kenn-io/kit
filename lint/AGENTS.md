# Lint Package Instructions

## Scope

`lint/` owns the shared Go lint policy for kenn-io repositories:

- `lint/config/golangci.yml` is the canonical golangci-lint configuration and
  `lint/config` merges a repository overlay into it.
- `lint/<name>/` packages are `go/analysis` analyzers; `lint.Analyzers()` is
  the complete list.
- `lint/sqlcheck` also exposes `Scan` for raw SQL; `kennlint sql` uses it on
  `.sql` files because golangci-lint only analyzes Go. It reports every
  `CHECK` constraint and `CREATE TYPE ... AS ENUM` outside comments and string
  literals; do not add shape-based exemptions, the policy is that validation
  rules do not live in the schema.
- `lint/gclplugin` registers the analyzers as the `kennlint` golangci-lint
  module plugin.
- `cmd/kennlint` runs the analyzers and renders or checks the configuration.

See `docs/adopting-kennlint.md` for the consumer workflow.

## Invariants

- Every analyzer is type-aware through `go/analysis` and proves its behavior
  with `analysistest` fixtures under `testdata/`, including negative cases.
  Do not replace an analyzer with text matching.
- Analyzers report only in the file kinds they document (test files for
  `sleeptest` and `testifyhelper` helper recommendations, all Go files for
  `testifyhelper` canonical names, plus `testutil` and `*test` helper packages
  for `sleeptest` unless `helper-packages` is off; non-test files for
  `nohttpmux`, `errtext` unless `include-tests` is set). Path policy beyond
  that belongs in golangci exclusions, not in analyzer code.
- `sleeptest` treats a bubble as the body of the function passed to
  `synctest.Test`, resolved by position for inline literals and by type
  object for functions passed by name. Helpers called from a bubble are
  reported on purpose; the fixture documents that limitation. The testify
  `Eventually` check stays off by default; consumers opt in per repository.
- Diagnostic strings are asserted in fixture `// want` comments; change both
  together.
- The canonical configuration must stay valid for the golangci-lint version in
  the kit `Makefile`, and `kennlint config` must render it without warnings.
  Adding a linter to the canonical config affects every consuming repository;
  measure the finding count on at least two consumers before adding one.
- Kit itself lints clean with the rendered configuration. Its overlay in
  `.golangci.overlay.yml` carries import aliases, the existing linter carve-outs,
  and the `paralleltest` scope for `selfupdate`, `tools/humacheck`, `logging`,
  and `tui/screen`; preserve that scope, require reasoned test-level suppressions
  for sequential shared-state cases, and do not add disables to it.
- `testifyhelper` requires canonical `assert` and `require` import and helper
  names. Requiring local helpers is opt-in through `require-helpers`, off
  by default for both libraries. Parent scopes retain package calls when local helpers would shadow
  package access in nested functions. Suggested fixes preserve object identity.
