// Package civiltime handles civil dates, day boundaries, and time-zone
// resolution for callers that place timestamps on calendar days.
//
// A Date is a calendar date with no zone and no instant. A DateTime is a wall
// clock with no offset. DayBounds turns a Date into the half-open interval of
// instants that make up that day in a zone, and DateOf goes the other way.
// ResolveLocal turns a wall clock in a zone into an instant with a stated rule
// for clocks that a daylight-saving change skips or repeats. ClassifyZone
// reads the zone text written next to a source timestamp and says whether the
// value names an instant at all.
//
// # Day boundaries
//
// The day for date D in zone Z starts at the first instant whose local date in
// Z is D or later and ends at the first instant whose local date is after D.
// Days are 23, 24, 25 hours or other lengths. Do not compute the start of a
// day as time.Date(y, m, d, 0, 0, 0, 0, loc): where a zone skips local
// midnight, time.Date normalizes the missing clock backward and returns 23:00
// on the previous day. In America/Santiago on 2026-09-06 and America/Havana
// on 2026-03-08 that puts the first hour of the previous evening into the
// wrong day. Adding 24 hours to a day start is wrong for the same reason.
//
// A date can be missing from a zone altogether: Pacific/Apia skipped
// 2011-12-30. DayBounds reports it with ok=false, and RangeBounds rejects it
// as an endpoint with ErrDateNotInZone while skipping it inside a range.
//
// # Wall clocks
//
// ResolveLocal resolves a wall clock that a zone repeats to the earlier
// instant and a wall clock that a zone skips to the first instant after the
// gap, and reports which happened. time.Date leaves both choices unspecified,
// and for a skipped clock it currently returns an instant before the gap.
//
// # Zone names
//
// LoadZone accepts "UTC" or a loadable IANA name containing a slash, such as
// America/Toronto or Etc/GMT+5. It rejects the empty string, "Local", numeric
// offsets, and slash-less legacy names such as EST, CET, EST5EDT, and GMT,
// because those read as abbreviations whose meaning depends on the reader.
//
// # Zone data
//
// The package does not embed a time-zone database. Zone loading uses the
// host's database through time.LoadLocation. Binaries that may run where the
// host has none, such as Windows or minimal containers, or that are built
// with -trimpath, should import time/tzdata in their main package.
package civiltime
