# PostHog

The reporter owns event admission, property filtering, identity and default
properties; HTTP and direct capture share Report. HTTP validates required
properties before enabled state. Capture-specific event rejection follows
method, content-type, bounded decoding and trailing-data checks, before Report.
The handler requires application/json so browsers preflight cross-origin posts,
and it bounds the body itself rather than trusting the caller's router.
Opted-out construction keeps the configured allowlist without creating a client.
Close waits at most ShutdownTimeout.

The package imports no OpenTelemetry, so products that only report events stay
small; TestPackageLinksNoOpenTelemetry guards this. Applications own route
registration, authentication, where the install file lives, when the
heartbeat runs and reporter shutdown.

The heartbeat checks wall-clock UTC dates hourly so system sleep does not turn
daily activity into a count of awake hours. Disabled clients start no ticker;
running heartbeats stop at the next hourly check after disabling. SDK logs use
the supplied slog logger (or slog.Default), preserving levels and the
application's log sink.

Daily claims hold a context-aware instance semaphore and file lock through durable
reservation, enqueue, and settlement. Retain the 35 most recently accepted UTC
dates per key in reservation order. Keep v1-compatible owner markers; retire
uncertainty after 35 later known acceptances. Tokens protect replacement claims
from stale cleanup by token-aware writers. Upgrade all writers sharing a claims
path together. If an old writer's rejected report leaves cleanup pending, 35 later
acceptances can retire its date. After a clock rollback reuses that date, the old
cleanup can delete the new claim and allow another count.
Sample time under the lock. Failed skip compaction warns and returns
StatusSkipped; failed acceptance settlement warns and returns StatusQueued, then
retries on the same instance's next call. Other storage failures return errors.
Release published reservations under the same lock, including durability errors.
Refuse numeric versions that may be at least 1 except literal `1`; reset damaged
content and return read errors other than a missing file. See daily.md for the
persisted format and recovery rules. Applications own finite property enums and
client retries. Queue acceptance does not guarantee network delivery.
