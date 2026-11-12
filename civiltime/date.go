package civiltime

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SegmentGap is the silence that splits a run of messages into separate
// segments: a gap of SegmentGap or more starts a new segment.
const SegmentGap = 30 * time.Minute

var (
	// ErrInvalidZone reports a zone name that LoadZone does not accept.
	ErrInvalidZone = errors.New("civiltime: invalid time zone")
	// ErrInvalidDate reports text or a value that is not a real civil date
	// or wall clock.
	ErrInvalidDate = errors.New("civiltime: invalid date")
	// ErrDateNotInZone reports a civil date that has no instant in a zone.
	ErrDateNotInZone = errors.New("civiltime: date does not exist in time zone")
)

// Date is a civil date in the proleptic Gregorian calendar, with no zone and
// no instant.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// ParseDate parses a strict YYYY-MM-DD date. The text must name a real
// calendar date; anything else returns an error wrapping ErrInvalidDate.
func ParseDate(s string) (Date, error) {
	d, ok := parseDate(s)
	if !ok {
		return Date{}, fmt.Errorf("%w: %q is not a YYYY-MM-DD calendar date", ErrInvalidDate, s)
	}
	return d, nil
}

func parseDate(s string) (Date, bool) {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return Date{}, false
	}
	year, ok1 := digits(s[0:4])
	month, ok2 := digits(s[5:7])
	day, ok3 := digits(s[8:10])
	if !ok1 || !ok2 || !ok3 {
		return Date{}, false
	}
	d := Date{Year: year, Month: time.Month(month), Day: day}
	if !d.valid() {
		return Date{}, false
	}
	return d, true
}

// digits parses a fixed-width run of ASCII digits.
func digits(s string) (int, bool) {
	n := 0
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

func (d Date) valid() bool {
	if d.Month < time.January || d.Month > time.December || d.Day < 1 {
		return false
	}
	return d.Day <= time.Date(d.Year, d.Month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// String formats d as YYYY-MM-DD. Years outside 0 through 9999 do not
// round-trip through ParseDate.
func (d Date) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, int(d.Month), d.Day)
}

// AddDays returns the date n days after d, or before it when n is negative.
// It is calendar arithmetic and involves no zone.
func (d Date) AddDays(n int) Date {
	return dateOf(d.midnight().AddDate(0, 0, n))
}

// Compare returns -1 if d is before o, 0 if they are the same date, and +1
// if d is after o.
func (d Date) Compare(o Date) int {
	switch {
	case d.Year != o.Year:
		return cmpInt(d.Year, o.Year)
	case d.Month != o.Month:
		return cmpInt(int(d.Month), int(o.Month))
	default:
		return cmpInt(d.Day, o.Day)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// midnight returns 00:00 on d as a UTC time. It is a wall-clock carrier for
// arithmetic, not the instant at which d starts anywhere but UTC.
func (d Date) midnight() time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

func dateOf(t time.Time) Date {
	y, m, day := t.Date()
	return Date{Year: y, Month: m, Day: day}
}

// DateOf returns the local date of instant t in loc. A nil loc means UTC.
func DateOf(t time.Time, loc *time.Location) Date {
	return dateOf(t.In(orUTC(loc)))
}

func orUTC(loc *time.Location) *time.Location {
	if loc == nil {
		return time.UTC
	}
	return loc
}

// DateTime is a wall clock with no offset: a civil date and a time of day.
type DateTime struct {
	Date
	Hour, Minute, Second, Nanosecond int
}

// ParseDateTime parses YYYY-MM-DDTHH:MM, optionally followed by :SS and a
// fraction of one to nine digits. It rejects any offset or zone suffix,
// including Z, and returns an error wrapping ErrInvalidDate.
func ParseDateTime(s string) (DateTime, error) {
	dt, ok := parseDateTime(s)
	if !ok {
		return DateTime{}, fmt.Errorf("%w: %q is not a YYYY-MM-DDTHH:MM[:SS[.fraction]] wall clock without offset", ErrInvalidDate, s)
	}
	return dt, nil
}

func parseDateTime(s string) (DateTime, bool) {
	if len(s) < 16 || s[10] != 'T' || s[13] != ':' {
		return DateTime{}, false
	}
	d, ok := parseDate(s[:10])
	if !ok {
		return DateTime{}, false
	}
	hour, ok1 := digits(s[11:13])
	minute, ok2 := digits(s[14:16])
	if !ok1 || !ok2 || hour > 23 || minute > 59 {
		return DateTime{}, false
	}
	dt := DateTime{Date: d, Hour: hour, Minute: minute}
	rest := s[16:]
	if rest == "" {
		return dt, true
	}
	if len(rest) < 3 || rest[0] != ':' {
		return DateTime{}, false
	}
	second, ok := digits(rest[1:3])
	if !ok || second > 59 {
		return DateTime{}, false
	}
	dt.Second = second
	rest = rest[3:]
	if rest == "" {
		return dt, true
	}
	frac := rest[1:]
	if rest[0] != '.' || len(frac) < 1 || len(frac) > 9 {
		return DateTime{}, false
	}
	n, ok := digits(frac)
	if !ok {
		return DateTime{}, false
	}
	for range 9 - len(frac) {
		n *= 10
	}
	dt.Nanosecond = n
	return dt, true
}

// String formats dt as YYYY-MM-DDTHH:MM:SS, followed by a fraction with
// trailing zeros removed when dt has a nonzero Nanosecond.
func (dt DateTime) String() string {
	s := fmt.Sprintf("%sT%02d:%02d:%02d", dt.Date, dt.Hour, dt.Minute, dt.Second)
	if dt.Nanosecond == 0 {
		return s
	}
	frac := strconv.Itoa(dt.Nanosecond + 1_000_000_000)[1:]
	return s + "." + strings.TrimRight(frac, "0")
}

// wall returns dt's wall clock as a UTC time, for arithmetic. Out-of-range
// fields normalize as they do in time.Date.
func (dt DateTime) wall() time.Time {
	return time.Date(dt.Year, dt.Month, dt.Day, dt.Hour, dt.Minute, dt.Second, dt.Nanosecond, time.UTC)
}
