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
  `sleeptest` and `testifyhelper`, non-test files for `nohttpmux`, `errtext`
  unless `include-tests` is set). Path policy beyond that belongs in golangci
  exclusions, not in analyzer code.
- Diagnostic strings are asserted in fixture `// want` comments; change both
  together.
- The canonical configuration must stay valid for the golangci-lint version in
  the kit `Makefile`, and `kennlint config` must render it without warnings.
  Adding a linter to the canonical config affects every consuming repository;
  measure the finding count on at least two consumers before adding one.
- Kit itself lints clean with the rendered configuration. Its overlay in
  `.golangci.overlay.yml` carries only import aliases and two documented
  carve-outs; do not add disables to it.
- `testifyhelper` accepts any helper name bound to `assert.New(t)` or
  `require.New(t)`. Use `assert`/`require` by default and a non-shadowing name
  such as `req` only when a nested subtest must reach the package to build its
  own helper.
