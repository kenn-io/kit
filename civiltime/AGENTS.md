# civiltime Instructions

## Scope

`civiltime/` holds pure functions over `time`: civil dates and wall clocks,
the instants that make up a day in a zone, zone-name validation, wall-clock
resolution, and classification of the zone text written on a source
timestamp. No wire structs, product vocabularies, configuration, or storage
live here; products own those and call these functions.

## Invariants

- A day is `[first instant whose local date is d or later, first instant
  whose local date is after d)`. `DayBounds` must agree with that definition
  for every date in every zone; `TestDayBoundsSweep` checks it against a
  brute-force scan. Never build a day from `time.Date(y, m, d, 0, 0, 0, 0,
  loc)` or `start.Add(24 * time.Hour)`.
- `DayOf(t, loc)` names the date whose `DayBounds` interval contains `t`;
  products place instants with it. `DateOf` stays the plain local date. They
  differ only where a zone set its clock back across midnight.
- A date a zone skipped (Pacific/Apia 2011-12-30) is `ok=false` with an empty
  interval at the next existing day's start, never an error from
  `DayBounds`.
- `ResolveLocal` takes the earlier instant in an overlap and the transition
  instant (first instant after the gap) in a gap. Do not fall back to
  `time.Date` for either case.
- `LoadZone` accepts exactly `UTC` or a loadable name containing `/`, and
  never `Local`, the empty string, or a slash-less legacy name.
- `ClassifyZone` never fails and never panics: unreadable text is
  `ZoneInvalid`. Email mode follows RFC 5322 section 4.3; general mode
  treats every alphabetic abbreviation except `UTC` and `Z` as `ZoneNamed`.
- No global mutable state. The package does not import `time/tzdata`;
  binaries that need an embedded database import it themselves, and test
  packages that load named zones import it in a `_test.go` file.
