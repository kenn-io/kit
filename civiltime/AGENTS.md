# civiltime Instructions

## Scope

`civiltime/` answers one question: which instants make up a calendar day in
a time zone. Dates are `cloud.google.com/go/civil` values; do not add a local
date or wall-clock type, parsing, zone-name policy, or timestamp-zone
classification here. Products own those.

## Invariants

- A day is `[first instant whose local date is d or later, first instant
  whose local date is after d)`. `TestDayBoundsSweep` checks `DayBounds`
  against a brute-force scan. Never build a day from `time.Date(y, m, d, 0,
  0, 0, 0, loc)` or `start.Add(24 * time.Hour)`.
- A date a zone skipped (Pacific/Apia 2011-12-30) is `ok=false` with an empty
  interval at the next day's start.
- The package does not import `time/tzdata`; test packages that load named
  zones import it in a `_test.go` file.
- Sweep coverage includes leap years beyond explicit TZif transitions. Go's
  POSIX-rule extension can return non-advancing `ZoneBounds` on December 31;
  the sampling walk must keep advancing without skipping transitions.
