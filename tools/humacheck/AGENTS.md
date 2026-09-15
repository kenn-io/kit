# Huma Checker Instructions

## Scope

`tools/humacheck/` is the type-aware checker behind `cmd/huma-check`. It
enforces the shared Huma contract across kenn-io Go modules:

- `jsonv2`: every `huma.Config` that reaches Huma (directly or through a
  module wrapper) carries an `application/json` `huma.Format` whose Marshal
  and Unmarshal reach `encoding/json/v2`, or the module overrides
  `huma.DefaultJSONFormat` in a package the constructing package imports.
- `spec`: OpenAPI 3 documents tracked in the repository are YAML, and a module
  that builds a Huma API commits at least one under its own module directory.
- `generator`: the standard client generator is declared somewhere in the
  repository: orval for TypeScript, oapi-codegen-dd v3 for Go. Any other
  generator is reported where it is declared so toolchains do not fragment;
  `// indirect` go.mod lines do not count as a declaration.
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
- Verdicts are three-valued. `no` needs a positively identified v1 codec or
  Huma default; anything the checker cannot follow is `unknown` and gets the
  "cannot verify" message, never the v1 message.
- The JSON v2 analysis is position-aware, not path-sensitive: the latest
  assignment before the construction call decides, a Formats mutation between
  that assignment and the call overrides it, every return of a config
  function must agree, and only `init` functions count as process-wide
  default overrides. A mutation inside one branch still counts; a mutation
  after the construction call never does.
- Route inventory composes adapter prefix + path and adapter prefix + group
  prefix + path. It does not track which API a route was registered on, so
  sibling APIs' prefixes can combine; keep that limitation documented in
  `collectRoutes` rather than adding pairwise prefix composition back.
- Requesters and registrars are flow sets of parameter indexes: a string
  parameter is interesting only when it reaches a URL or path position. Only
  those argument positions are inspected at call sites.
- Repository rules read file contents from the Git index through
  `indexFS`, never from the working tree, so a pre-commit run judges what
  will be committed. The directory walk is only for checkouts outside Git.
- Git subprocesses run with inherited `GIT_DIR`/`GIT_INDEX_FILE`/
  `GIT_WORK_TREE` stripped (`gitenv.StripInherited`) so the checker binds to
  the directory it was given; a parent hook exporting those variables must
  not redirect it, and tests under such a hook would otherwise read the outer
  repository.
- Generator declarations are matched as whole tokens on non-comment lines;
  Go files contribute only `//go:generate` directives and go.mod skips
  `// indirect` lines.

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
