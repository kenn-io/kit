<h1 align="center">kit</h1>

<p align="center">
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-1.27.3+-00ADD8?logo=go" alt="Go 1.27.3+"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-blue.svg" alt="License: Apache 2.0"></a>
  <a href="https://pkg.go.dev/go.kenn.io/kit"><img src="https://pkg.go.dev/badge/go.kenn.io/kit.svg" alt="Go Reference"></a>
</p>

<p align="center">
  <a href="https://pkg.go.dev/go.kenn.io/kit">API Reference</a> ·
  <a href="docs/README.md">Documentation</a> ·
  <a href="AGENTS.md">Agent Guide</a>
</p>

**Reusable Go building blocks for Kenn CLI and developer tools.**

kit is the shared foundation under kenn-io applications such as kata,
roborev, docbank, agentsview, and forge. Packages are small, app-neutral, and
usable by more than one caller: no product-specific config, database state, UI
behavior, or provider workflows live here.

## What you can do

- **Build agent-aware CLIs.** `agentcli` builds command lines for supported
  coding-agent CLIs, `agenthook` installs hooks into agent harnesses,
  `agentmcp` registers MCP servers in agent configuration files,
  `agentsetup` runs idempotent setup items and reports their readiness, and
  `clisearch` finds runnable commands from task descriptions.
- **Store content safely.** `packstore` keeps a mixed loose-and-packed
  content-addressed store, `pack` implements the sealed-immutable pack
  container format, and `backup` snapshots metadata plus content-addressed
  file trees.
- **Write files without corruption.** `atomicfile` writes so readers see old
  or new content, never a partial file; `safefileio` hardens file I/O for
  local runtime state.
- **Run local daemons.** `daemon` provides reusable infrastructure for the
  background daemons behind CLI tools.
- **Add embeddings and vector search.** `embedclient`, `embedconfig`,
  `embedfit`, `embedlocal`, `embedmodel`, and `embedserver` cover endpoints,
  token budgets, and a local embedding server; `vector` searches the
  resulting vectors backend-neutrally.
- **Handle filesystems portably.** `fslink` classifies and creates symlinks
  and Windows junctions, `fsname` cleans names for every modern OS and file
  system, and `pathresolve` canonicalizes paths for containment checks.
- **Place times on calendar days.** `civiltime` gives the instants that make
  up a civil date in an IANA zone, including days that skip midnight or a
  whole date, resolves wall clocks that daylight-saving changes skip or
  repeat, and classifies the zone text written on a source timestamp.
- **Share development tooling.** `lint` owns the shared Go lint policy,
  `logging` builds reusable slog loggers, `openssh` orchestrates an OpenSSH
  client, `telemetry` wraps OpenTelemetry and anonymous usage events, and
  `spill` moves oversized in-memory lists to a temporary file.

## Installation

```sh
go get go.kenn.io/kit@latest
```

kit requires Go 1.27.3 or later. Import the packages you need; each carries
its own Go documentation and package-level agent guidance.

## Documentation

- [API reference](https://pkg.go.dev/go.kenn.io/kit) on pkg.go.dev

### Development

- [Developing kit](docs/development.md)

### Adoption

- [Task-based command discovery](clisearch/README.md)
- [Local embedding server installation](docs/kennembed.md)
- [Adopting packed content storage](docs/adopting-packstore.md)
- [Adopting the shared Go lint policy (kennlint)](docs/adopting-kennlint.md)
- [Daily screen reports](telemetry/posthog/daily.md)
- [Confirmed milestone delivery](telemetry/posthog/delivery.md)
- [PostHog event fields](telemetry/posthog/fields.md)

### Architecture

- [Streaming pack I/O](docs/architecture/streaming-pack-io.md)

### Formats

- [Backup repository and pack format](backup/FORMAT.md)

## Agent guide

Coding agents working in this repository should read [AGENTS.md](AGENTS.md).

## License

kit is licensed under the [Apache License, Version 2.0](LICENSE).
