package civiltime

import (
	"fmt"
	"strings"
	"time"
)

// LoadZone loads a zone a caller named explicitly. It accepts exactly "UTC"
// or an IANA name that contains a slash and that time.LoadLocation can load,
// such as America/Toronto or Etc/GMT+5. It rejects the empty string,
// "Local", numeric offsets, and slash-less legacy names such as EST, CET,
// EST5EDT, and GMT, even where the zone database has them, because they read
// as abbreviations. Names under posix/ and right/ are rejected too: some
// hosts ship them and others do not, and right/ zones count leap seconds.
// Errors wrap ErrInvalidZone.
func LoadZone(name string) (*time.Location, error) {
	if name == "UTC" {
		return time.UTC, nil
	}
	if !strings.Contains(name, "/") || strings.HasPrefix(name, "posix/") || strings.HasPrefix(name, "right/") {
		return nil, fmt.Errorf("%w: %q: want an IANA name such as America/Toronto, or UTC", ErrInvalidZone, name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", ErrInvalidZone, name, err)
	}
	return loc, nil
}

// searchWindow bounds the instants examined around a civil date or wall
// clock. Real offsets stay within about ±15 hours, and the largest recorded
// jump of a local date is a full day, so 50 hours on each side covers every
// instant that can carry a given local date plus the next day's start.
const searchWindow = 50 * time.Hour

// maxSpans caps the constant-offset intervals examined in one window. Real
// zones change offset a few times a year at most.
const maxSpans = 64

// span is an interval of instants [start, end) over which loc keeps one
// UTC offset, so local wall-clock time is monotone across it.
type span struct {
	start, end time.Time
	offset     time.Duration
}

// spans splits [lo, hi) into constant-offset intervals of loc.
func spans(loc *time.Location, lo, hi time.Time) []span {
	var out []span
	for t := lo; t.Before(hi); {
		local := t.In(loc)
		_, offset := local.Zone()
		_, end := local.ZoneBounds()
		if end.IsZero() || !end.After(t) || end.After(hi) || len(out) == maxSpans-1 {
			end = hi
		}
		out = append(out, span{start: t, end: end, offset: time.Duration(offset) * time.Second})
		t = end
	}
	return out
}

// firstAtOrAfter returns the first instant whose local date in loc is d or
// later.
func firstAtOrAfter(d Date, loc *time.Location) (time.Time, bool) {
	midnight := d.midnight()
	for _, s := range spans(loc, midnight.Add(-searchWindow), midnight.Add(searchWindow)) {
		// Local wall time inside s is instant+offset, so local midnight of d
		// falls at midnight-offset. If s starts later, its first instant
		// already shows d or a later date.
		at := midnight.Add(-s.offset)
		if at.Before(s.start) {
			at = s.start
		}
		if at.Before(s.end) {
			return at.In(loc), true
		}
	}
	return time.Time{}, false
}

// exists reports whether any instant has local date d in loc.
func exists(d Date, loc *time.Location) bool {
	midnight := d.midnight()
	next := midnight.AddDate(0, 0, 1)
	for _, s := range spans(loc, midnight.Add(-searchWindow), midnight.Add(searchWindow)) {
		if s.start.Add(s.offset).Before(next) && s.end.Add(s.offset).After(midnight) {
			return true
		}
	}
	return false
}

// DayBounds returns the half-open interval [start, end) of instants that make
// up date d in loc: start is the first instant whose local date is d, and end
// is the first instant whose local date is after d. A nil loc means UTC.
//
// When d is not a real calendar date or no instant in loc has local date d,
// ok is false. For a date the zone skipped, start and end are both the first
// instant after the skipped date, so the interval is empty.
//
// The day can be shorter or longer than 24 hours and need not start at
// 00:00 local time: in America/Santiago on 2026-09-06 it starts at 01:00.
//
// Consecutive days share their boundary, so the days of a zone partition
// time. Where a zone once set its clock back across midnight
// (America/St_Johns until 2011 changed at 00:01), the instants after the
// change show date d again after d+1 has begun; they belong to d+1 here,
// while DateOf reports d for them. DayOf gives the day an instant belongs
// to under this rule.
func DayBounds(d Date, loc *time.Location) (start, end time.Time, ok bool) {
	loc = orUTC(loc)
	if !d.valid() {
		return time.Time{}, time.Time{}, false
	}
	start, found := firstAtOrAfter(d, loc)
	if !found {
		return time.Time{}, time.Time{}, false
	}
	end, found = firstAtOrAfter(d.AddDays(1), loc)
	if !found {
		return time.Time{}, time.Time{}, false
	}
	return start, end, exists(d, loc)
}

// DayOf returns the day instant t belongs to in loc: the date whose DayBounds
// interval contains t. A nil loc means UTC. It equals DateOf(t, loc) except
// where a zone set its clock back across midnight and showed the previous
// date again after the next day began (America/St_Johns until 2011): there
// DateOf reports the earlier date and DayOf the later one.
func DayOf(t time.Time, loc *time.Location) Date {
	loc = orUTC(loc)
	d := DateOf(t, loc)
	// t shows d, so it is never before d's start; step forward past days
	// that ended before it. Real zones need at most one step.
	for range 3 {
		if _, end, _ := DayBounds(d, loc); t.Before(end) {
			return d
		}
		d = d.AddDays(1)
	}
	return d
}

// RangeBounds returns the half-open interval of instants covering the
// inclusive civil range from..to in loc: from the start of from to the end of
// to. A nil loc means UTC. Dates the zone skipped inside the range are
// covered by an empty interval and need no handling. An endpoint the zone
// skipped returns an error wrapping ErrDateNotInZone; an invalid date or a
// from after to returns an error wrapping ErrInvalidDate.
func RangeBounds(from, to Date, loc *time.Location) (start, end time.Time, err error) {
	loc = orUTC(loc)
	if !from.valid() || !to.valid() {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: range %s..%s", ErrInvalidDate, from, to)
	}
	if from.Compare(to) > 0 {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: range start %s is after its end %s", ErrInvalidDate, from, to)
	}
	start, _, ok := DayBounds(from, loc)
	if !ok {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: %s in %s", ErrDateNotInZone, from, loc)
	}
	_, end, ok = DayBounds(to, loc)
	if !ok {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: %s in %s", ErrDateNotInZone, to, loc)
	}
	return start, end, nil
}

// Resolution says how ResolveLocal mapped a wall clock to an instant.
type Resolution int

const (
	// Unique means exactly one instant shows the wall clock.
	Unique Resolution = iota
	// Overlap means the zone repeats the wall clock, as when clocks fall
	// back, and the earlier instant was chosen.
	Overlap
	// Gap means the zone skips the wall clock, as when clocks spring
	// forward, and the first instant after the gap was chosen.
	Gap
)

// String returns "unique", "overlap", or "gap".
func (r Resolution) String() string {
	switch r {
	case Unique:
		return "unique"
	case Overlap:
		return "overlap"
	case Gap:
		return "gap"
	default:
		return fmt.Sprintf("Resolution(%d)", int(r))
	}
}

// ResolveLocal returns the instant at which loc shows wall clock dt. A nil
// loc means UTC. When loc shows dt twice, it returns the earlier instant and
// Overlap. When loc never shows dt, it returns the first instant after the
// gap, which is the transition itself, and Gap.
//
// This differs from time.Date, which does not guarantee its choice for a
// skipped clock and today applies the offset in force before the gap: for
// America/Toronto 2026-03-08T02:30 it returns 01:30 EST (06:30Z), an instant
// before the gap, while ResolveLocal returns 03:00 EDT (07:00Z). Fields of dt
// outside their ranges normalize as they do in time.Date.
func ResolveLocal(dt DateTime, loc *time.Location) (time.Time, Resolution) {
	loc = orUTC(loc)
	wall := dt.wall()
	ss := spans(loc, wall.Add(-searchWindow), wall.Add(searchWindow))

	var first time.Time
	matches := 0
	for _, s := range ss {
		at := wall.Add(-s.offset)
		if !at.Before(s.start) && at.Before(s.end) {
			if matches == 0 {
				first = at
			}
			matches++
		}
	}
	switch {
	case matches == 1:
		return first.In(loc), Unique
	case matches > 1:
		return first.In(loc), Overlap
	}

	// The wall clock falls between the last local time before a transition
	// and the first local time after it.
	for i := 0; i+1 < len(ss); i++ {
		boundary := ss[i].end
		if !wall.Before(boundary.Add(ss[i].offset)) && wall.Before(boundary.Add(ss[i+1].offset)) {
			return boundary.In(loc), Gap
		}
	}
	// Unreachable for real zone data; fall back to the time package's rule.
	return time.Date(dt.Year, dt.Month, dt.Day, dt.Hour, dt.Minute, dt.Second, dt.Nanosecond, loc), Gap
}
