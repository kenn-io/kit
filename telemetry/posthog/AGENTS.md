# PostHog

The reporter owns event admission, property filtering, identity and default
properties; HTTP and direct capture share Report. HTTP validates required
properties before enabled state. The handler requires application/json so
browsers preflight cross-origin posts, and it bounds the body itself rather
than trusting the caller's router. Opted-out construction keeps the configured
allowlist without creating a client. Close waits at most ShutdownTimeout.

The package imports no OpenTelemetry, so products that only report events stay
small; TestPackageLinksNoOpenTelemetry guards this. Applications own route
registration, authentication, where the install file lives, when the
heartbeat runs and reporter shutdown.

The heartbeat checks wall-clock UTC dates hourly so system sleep does not turn
daily activity into a count of awake hours. SDK logs use the supplied slog
logger (or slog.Default), preserving levels and the application's log sink.

Daily claims hold a context-aware file lock through queue acceptance and
rollback. Reserve durably before enqueue; retain accepted dates and deduplicate
by membership. Reservation and enqueue use one time sampled under the lock.
Rejected captures release their reservation; storage failures return errors.
Failed reservation saves release published claims under the same lock.
Pending cleanup follows visible publication, including durability failures.
Damaged claims reset to empty state; newer format versions remain untouched.
Daily events require a nonblank installation identity even when opted out.
Applications own finite property enums and client retries. Queue acceptance does
not guarantee eventual network delivery.
