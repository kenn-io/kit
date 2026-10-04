# PostHog

The reporter owns event admission, property filtering, identity and default
properties; the HTTP capture handler checks admission before enabled state and
calls Capture for everything else. The handler requires application/json so
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
