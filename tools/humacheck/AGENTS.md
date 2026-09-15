# Huma Checker Instructions

## Scope

`tools/humacheck/` is the type-aware checker behind `cmd/huma-check`. It
enforces the shared Huma contract across kenn-io Go modules:

- `jsonv2`: every `huma.Config` that reaches Huma (directly or through a
  module wrapper) carries an `application/json` `huma.Format` whose Marshal
  and Unmarshal reach `encoding/json/v2`, or the module overrides
  `huma.DefaultJSONFormat` in a package the constructing package imports.
- `spec`: OpenAPI 3 documents tracked in the repository are YAML, and a module
  that builds a Huma API commits at least one.
- `generator`: a supported client generator is declared somewhere in the
  repository, and banned generators are reported where they appear.
- `client`: string literals naming one of the module's own routes are reported
  when they flow into request-building code.
- `frontend`: the same route match over `fetch`-style calls in browser sources.

`cmd/huma-check` only wraps `Run` for CLI use.

## Analyzer Rules

- Keep the Go rules type-aware through `go/packages` and `go/types`; never
  fall back to text matching for Go code. Repository rules (`spec`,
  `generator`, `frontend`) are intentionally text-based over tracked files.
- Loading uses one `packages.Load` over the caller's patterns so functions in
  any module package can be followed by `*types.Func` identity. Do not switch
  to `go/analysis` facts: the client rule needs route inventory from packages
  the client does not import.
- Tests (`_test.go`), generated files (`ast.IsGenerated`), and `generated/`
  directories are never judged. Extend `skipFile` rather than adding per-rule
  exceptions.
- Verdicts are three-valued. Report `no` and `unknown` separately so a
  config the checker cannot follow says so instead of pretending it is v1.
- Route matching is exact per prefix: `prefix + path` with `{param}` and
  format verbs as single segments and a trailing `/` as at least one more
  segment. A literal made only of placeholders never matches.
- Requesters and registrars are fixpoints over "forwards a string parameter";
  keep both symmetric when changing either.

## Tests

- Go fixtures live under `testdata/src` in GOPATH mode with stub `huma` and
  `humago` packages. Assert diagnostics with `// want "regexp"` comments; the
  comment is not a Go string, so escape regexp metacharacters once.
- Repository rules are tested with `testing/fstest` maps; never depend on git
  state or the user's environment.
- When a diagnostic message changes, update the fixture comments and
  `repo_test.go` expectations in the same change.
- Validate precision changes against real kenn-io repositories before
  relaxing or tightening a rule; the fixtures cover shapes seen there.
