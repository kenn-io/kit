// Package civiltime gives the instants that make up a calendar day in a time
// zone. Dates are cloud.google.com/go/civil values.
//
// The day for date D in zone Z runs from the first instant whose local date in
// Z is D to the first instant whose local date is after D. Days can be 23, 25
// or other numbers of hours long. Do not compute the start of a day as
// time.Date(y, m, d, 0, 0, 0, 0, loc): where a zone skips local midnight,
// time.Date returns 23:00 on the previous day (America/Santiago on
// 2026-09-06, America/Havana on 2026-03-08), and where a zone repeats
// midnight it can return the second one. Adding 24 hours to a day start is
// wrong for the same reason.
//
// The package does not embed a time-zone database. Binaries that may run
// where the host has none, such as Windows or minimal containers, should
// import time/tzdata in their main package.
package civiltime
