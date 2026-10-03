---
title: "Telemetry package invariants"
description: "Ownership rules for telemetry admission, property filtering, identities, HTTP capture, opt-out behavior, and application wiring."
last_edited: 2026-10-02
---
# Telemetry

The reporter owns event admission, property filtering, identity and default
properties; the HTTP capture handler checks admission before enabled state and
calls Capture for everything else. The handler requires application/json so
browsers preflight cross-origin posts, and it bounds the body itself rather
than trusting the caller's router. Opted-out construction keeps the configured
allowlist without creating a client. Applications own route registration,
authentication and reporter shutdown.
