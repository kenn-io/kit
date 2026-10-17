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

Daily claims take a context-aware instance semaphore and file lock and hold
both through queue acceptance and rollback. Reserve durably before enqueue;
retain the 35 most recently accepted UTC dates per key in acceptance order.
Keep at most 35 pending reservations per key; a new distinct date at the limit
returns an error before enqueue, preserving existing claims. Abandoned claims
block new dates until their owners settle or the caller replaces the state
file, which forgets all deduplication history. Settlement
preserves reservation order across writers. Reservation and enqueue use one
time sampled under the lock. Skipped-report compaction is best effort and logs
failures. After queue acceptance, settlement storage failures warn through the
configured logger and return StatusQueued; retry unpublished settlement on the
next call. Other storage failures return errors.
Failed reservation saves release published claims under the same lock.
Pending cleanup follows visible publication, including durability failures.
Version 1 dates migrate as accepted records to version 2. Every writer sharing
the claims path must support version 2; older readers refuse the file and stop
reporting daily events. Numeric versions other
than the exact literals `1` and `2` that may be at least 1 are refused unchanged.
Other unreadable content resets to empty claims. Read errors other than a
missing file are returned.
Applications own finite property enums and client retries. Queue acceptance does
not guarantee eventual network delivery.
