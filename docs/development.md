# Developing kit

## Shared Go build cache

`make` targets build with `-trimpath`, so compiled code records
module-relative source paths (`go.kenn.io/kit/pack/pack.go`) instead of the
absolute path of your checkout. Every checkout and git worktree of kit then
shares one set of Go build cache entries instead of compiling and caching its
own copy of every package.

- The Makefile adds `-trimpath` to any `GOFLAGS` you already set, including a
  value saved with `go env -w GOFLAGS=...`.
- To opt out for one command, run it with `GOFLAGS=-trimpath=false`.
- To get the same sharing from plain `go build` and `go test` outside `make`,
  run `go env -w GOFLAGS=-trimpath` once.

## Debugging trimmed builds

A trimmed binary does not record where its source files live on disk, so Delve
cannot find them without help.

- `dlv debug` and `dlv test` inherit `GOFLAGS`. Pass
  `--build-flags=-trimpath=false` so the debug build records real paths. Debug
  builds disable optimizations, so they never shared cache entries anyway.
- `dlv exec` and `dlv attach` on a trimmed binary: start Delve from the
  repository root and run `config substitute-path -guess` (Delve 1.24 or
  later). It maps the module path to your checkout, the standard library to
  `GOROOT`, and dependencies to the module cache.
