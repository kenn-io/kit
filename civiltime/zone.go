package civiltime

import (
	"time"

	"cloud.google.com/go/civil"
)

// DayBounds returns the half-open interval [start, end) of instants that make
// up date d in loc: start is the first instant whose local date is d, and end
// is the first instant whose local date is after d. A nil loc means UTC.
//
// The day can be shorter or longer than 24 hours and need not start at local
// 00:00: in America/Santiago on 2026-09-06 it starts at 01:00.
//
// ok is false when d is not a valid date, or when loc skipped d altogether
// (Pacific/Apia 2011-12-30). For a skipped date, start and end are both the
// start of the next day, so the interval is empty.
func DayBounds(d civil.Date, loc *time.Location) (start, end time.Time, ok bool) {
	if !d.IsValid() {
		return time.Time{}, time.Time{}, false
	}
	if loc == nil {
		loc = time.UTC
	}
	start = dayStart(d, loc)
	end = dayStart(d.AddDays(1), loc)
	return start, end, civil.DateOf(start) == d
}

// dayStart returns the first instant whose local date in loc is d or later.
// time.Date alone is wrong in two cases: where a zone skips midnight it
// returns 23:00 on the previous day, and where a zone repeats midnight it may
// return the second one.
func dayStart(d civil.Date, loc *time.Location) time.Time {
	t := time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, loc)
	if civil.DateOf(t).Before(d) {
		// Midnight fell in a gap: the day starts where the gap ends.
		if _, gapEnd := t.ZoneBounds(); !gapEnd.IsZero() {
			t = gapEnd
		}
	}
	if zoneStart, _ := t.ZoneBounds(); !zoneStart.IsZero() {
		// If the zone in effect before t already showed d, midnight repeated
		// and the day began under that earlier offset.
		if prev := zoneStart.Add(-time.Nanosecond).In(loc); !civil.DateOf(prev).Before(d) {
			_, offset := prev.Zone()
			utcMidnight := time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
			t = utcMidnight.Add(-time.Duration(offset) * time.Second).In(loc)
		}
	}
	return t
}
