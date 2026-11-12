package civiltime

import (
	"fmt"
	"strings"
	"time"
)

// Precision is how finely a time value is known. Its values are the wire
// strings "year", "month", "date", "hour", "minute", "second", and
// "fraction".
type Precision string

// Precisions, coarsest first.
const (
	PrecisionYear     Precision = "year"
	PrecisionMonth    Precision = "month"
	PrecisionDate     Precision = "date"
	PrecisionHour     Precision = "hour"
	PrecisionMinute   Precision = "minute"
	PrecisionSecond   Precision = "second"
	PrecisionFraction Precision = "fraction"
)

// ParsePrecision returns the Precision named by s, or an error for any other
// string.
func ParsePrecision(s string) (Precision, error) {
	switch p := Precision(s); p {
	case PrecisionYear, PrecisionMonth, PrecisionDate, PrecisionHour,
		PrecisionMinute, PrecisionSecond, PrecisionFraction:
		return p, nil
	}
	return "", fmt.Errorf("civiltime: unknown precision %q", s)
}

// ZoneState says what the zone text written next to a source timestamp tells
// a reader. Its values are the wire strings "utc", "offset", "iana",
// "rfc5322", "named", "omitted", and "invalid".
type ZoneState string

// Zone states. The first four make the value an instant; the rest leave it a
// floating wall clock.
const (
	// ZoneUTC is UTC written as Z, UTC, +00:00, or +0000.
	ZoneUTC ZoneState = "utc"
	// ZoneOffset is a numeric offset such as -04:00 or +0530.
	ZoneOffset ZoneState = "offset"
	// ZoneIANA is a loadable IANA zone name such as America/New_York.
	ZoneIANA ZoneState = "iana"
	// ZoneRFC5322 is one of the obsolete zone names RFC 5322 section 4.3
	// gives fixed offsets: UT, GMT, EST, EDT, CST, CDT, MST, MDT, PST, PDT.
	ZoneRFC5322 ZoneState = "rfc5322"
	// ZoneNamed is a zone name whose offset the reader cannot know, such as
	// CET, IST, or BST.
	ZoneNamed ZoneState = "named"
	// ZoneOmitted means no zone was written, or the source says the offset
	// is unknown.
	ZoneOmitted ZoneState = "omitted"
	// ZoneInvalid is zone text that is malformed or names no known zone.
	ZoneInvalid ZoneState = "invalid"
)

// Exact reports whether s makes a value an instant: utc, offset, iana, or
// rfc5322.
func (s ZoneState) Exact() bool {
	switch s {
	case ZoneUTC, ZoneOffset, ZoneIANA, ZoneRFC5322:
		return true
	default:
		return false
	}
}

// Mode selects the rules ClassifyZone applies.
type Mode int

const (
	// General applies to every source except email headers.
	General Mode = iota
	// Email applies to RFC 5322 Date: header values and the dates in
	// Received: headers.
	Email
)

// Designator is the result of reading a source timestamp's zone text.
type Designator struct {
	// State is what the zone text tells a reader.
	State ZoneState
	// Text is the zone text as the caller passed it.
	Text string
	// OffsetSeconds is the UTC offset when State is ZoneUTC, ZoneOffset,
	// or ZoneRFC5322, and zero otherwise.
	OffsetSeconds int
	// Location is the loaded zone when State is ZoneIANA, and nil
	// otherwise.
	Location *time.Location
}

// maxOffset is the largest numeric offset ClassifyZone accepts.
const maxOffset = 18 * 3600

// rfc5322Offsets holds the obsolete zone names of RFC 5322 section 4.3 with
// their fixed offsets in hours.
var rfc5322Offsets = map[string]int{
	"UT": 0, "GMT": 0,
	"EST": -5, "EDT": -4,
	"CST": -6, "CDT": -5,
	"MST": -7, "MDT": -6,
	"PST": -8, "PDT": -7,
}

// ClassifyZone reads the zone text written next to a source timestamp. Text
// is trimmed of surrounding white space before it is read; Designator.Text
// keeps it as given. It never fails: text it cannot read is ZoneInvalid.
//
// In General mode, Z, UTC, +00:00, +0000, -00:00, and -0000 are ZoneUTC
// (letters in any case); a numeric offset of the form ±hh:mm or ±hhmm up to
// 18 hours is ZoneOffset; a name LoadZone accepts is ZoneIANA; any other
// run of ASCII letters is ZoneNamed; empty text is ZoneOmitted.
//
// Email mode follows RFC 5322. +0000 is ZoneUTC but -0000 means the offset
// is unknown and is ZoneOmitted, as are the single-letter military zones,
// Z included. UT, GMT, and the eight North American names are ZoneRFC5322
// with their fixed offsets. An IANA name is not legal in a Date: header and
// is ZoneNamed. UTC is ZoneUTC in both modes.
func ClassifyZone(text string, mode Mode) Designator {
	d := Designator{Text: text}
	zone := strings.TrimSpace(text)
	upper := strings.ToUpper(zone)
	switch {
	case zone == "":
		d.State = ZoneOmitted
	case upper == "UTC":
		d.State = ZoneUTC
	case zone[0] == '+' || zone[0] == '-':
		offset, ok := parseOffset(zone)
		switch {
		case !ok:
			d.State = ZoneInvalid
		case offset == 0 && zone[0] == '-' && mode == Email:
			d.State = ZoneOmitted
		case offset == 0:
			d.State = ZoneUTC
		default:
			d.State, d.OffsetSeconds = ZoneOffset, offset
		}
	case strings.Contains(zone, "/"):
		loc, err := LoadZone(zone)
		switch {
		case err != nil:
			d.State = ZoneInvalid
		case mode == Email:
			d.State = ZoneNamed
		default:
			d.State, d.Location = ZoneIANA, loc
		}
	case !isLetters(zone):
		d.State = ZoneInvalid
	case mode == Email && len(zone) == 1 && upper != "J":
		// RFC 5322 section 4.3: military zones are treated as -0000.
		d.State = ZoneOmitted
	case mode == General && upper == "Z":
		d.State = ZoneUTC
	case mode == Email:
		if hours, ok := rfc5322Offsets[upper]; ok {
			d.State, d.OffsetSeconds = ZoneRFC5322, hours*3600
		} else {
			d.State = ZoneNamed
		}
	default:
		d.State = ZoneNamed
	}
	return d
}

// parseOffset parses ±hh:mm or ±hhmm with minutes below 60 and a magnitude
// of at most 18 hours.
func parseOffset(s string) (int, bool) {
	body := s[1:]
	if len(body) == 5 && body[2] == ':' {
		body = body[:2] + body[3:]
	}
	if len(body) != 4 {
		return 0, false
	}
	hours, ok1 := digits(body[:2])
	minutes, ok2 := digits(body[2:])
	if !ok1 || !ok2 || minutes > 59 {
		return 0, false
	}
	offset := hours*3600 + minutes*60
	if offset > maxOffset {
		return 0, false
	}
	if s[0] == '-' {
		offset = -offset
	}
	return offset, true
}

func isLetters(s string) bool {
	for i := range len(s) {
		c := s[i] | 0x20
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

// Resolve turns wall clock dt, written with zone designator d, into an
// instant. For an exact state it returns the instant in UTC and exact=true:
// fixed offsets subtract the offset, and an IANA zone goes through
// ResolveLocal, whose Resolution it returns. For every other state the value
// is a floating wall clock: Resolve returns the zero time, exact=false, and
// Unique, which then carries no meaning.
func Resolve(dt DateTime, d Designator) (at time.Time, exact bool, res Resolution) {
	switch d.State {
	case ZoneUTC, ZoneOffset, ZoneRFC5322:
		return dt.wall().Add(-time.Duration(d.OffsetSeconds) * time.Second), true, Unique
	case ZoneIANA:
		if d.Location == nil {
			return time.Time{}, false, Unique
		}
		at, res := ResolveLocal(dt, d.Location)
		return at.UTC(), true, res
	default:
		return time.Time{}, false, Unique
	}
}
