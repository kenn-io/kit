---
title: "Telemetry"
description: "Invariants for telemetry consent, event delivery, and package boundaries."
last_edited: 2026-10-04
---

# Telemetry

This package holds the OpenTelemetry setup. The PostHog reporter lives in
`posthog`, which must never import OpenTelemetry; the PostHog names here are
deprecated aliases for existing importers.
