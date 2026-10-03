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
