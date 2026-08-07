# Daemon Package Instructions

## Scope

`daemon/` owns shared lifecycle pieces for local background daemons: endpoint
parsing, loopback or Unix-socket clients, runtime records, liveness probes,
listen locks, and caller-driven auto-start. Application server setup, auth,
databases, command parsing, and shutdown policy belong to the caller.

## Invariants

- Never infer live daemon state from a runtime record alone. Probe the endpoint
  before claiming a process is reachable.
- Treat process-creation identity as opaque and exact-match only. An unknown
  identity never authorizes destructive action against a process or record.
- Never send a bearer credential to a runtime-record endpoint before it proves
  possession of the daemon token and its recorded service, version, PID,
  network, and address. Use the package proof ping and probe contract for this
  ordering.
- Default network behavior stays local. Do not add public listen addresses or
  broad bind behavior without an explicit caller option.
- Unix sockets and runtime records must live under private current-user
  directories.
- Do not remove an existing path unless it is known to be the stale Unix socket
  this package created. Refuse paths whose type or ownership does not match
  that intent.
- Use listen locks to serialize startup and bind attempts.
- Hold the owner lock for the daemon's full writable lifetime; use the start
  lock only to serialize discovery, replacement, and launch decisions.
- Route every `Manager.Ensure` lookup through `Manager.Find` so a caller's
  `FindFunc` is used for initial discovery, locked re-discovery, and polling.
- Keep platform-specific behavior in build-tagged files when ownership, sockets,
  process checks, or permissions differ.
- On Windows, an openable process object is not proof that the process is still
  running; retained handles outlive termination, so liveness must check the
  process exit status for `STILL_ACTIVE`.
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
