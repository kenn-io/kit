# Daemon Package Instructions

## Scope

`daemon/` owns shared lifecycle pieces for local background daemons: endpoint
parsing, loopback or Unix-socket clients, runtime records, liveness probes,
listen locks, and caller-driven auto-start. Application server setup, auth,
databases, command parsing, and shutdown policy belong to the caller.

## Invariants

- Never infer live daemon state from a runtime record alone. Probe the endpoint
  before claiming a process is reachable.
- Default network behavior stays local. Do not add public listen addresses or
  broad bind behavior without an explicit caller option.
- Unix sockets and runtime records must live under private current-user
  directories.
- Do not remove an existing path unless it is known to be the stale Unix socket
  this package created. Refuse paths whose type or ownership does not match
  that intent.
- Use listen locks to serialize startup and bind attempts.
- Keep platform-specific behavior in build-tagged files when ownership, sockets,
  process checks, or permissions differ.
- Auto-start goes through the caller-provided `StartFunc`; this package must not
  invent application launch commands.
- Windows detached children use `DETACHED_PROCESS`, not `CREATE_NO_WINDOW`.
  Hidden consoles expose `CONIN$`, which can make terminal-probing libraries
  block forever at daemon startup. Non-interactive console-subsystem
  descendants that must avoid visible windows should set `CREATE_NO_WINDOW` at
  their own spawn site.

## Tests

- Use local HTTP handlers, temporary dirs, and per-test runtime stores.
- Assert what a client observes: status code, response body, runtime record, or
  returned error.
- Avoid timing assumptions. Prefer explicit locks, probes, and short contexts
  over sleeps.
- Do not require elevated privileges or external daemons.
