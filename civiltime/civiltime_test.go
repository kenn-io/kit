package civiltime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := LoadZone(name)
	require.NoError(t, err)
	return loc
}

func mustDate(t *testing.T, s string) Date {
	t.Helper()
	d, err := ParseDate(s)
	require.NoError(t, err)
	return d
}

func mustDateTime(t *testing.T, s string) DateTime {
	t.Helper()
	dt, err := ParseDateTime(s)
	require.NoError(t, err)
	return dt
}

func mustInstant(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, s)
	require.NoError(t, err)
	return at
}

func TestDayBoundsVectors(t *testing.T) {
	tests := []struct {
		zone, date string
		start, end string
		hours      int
	}{
		{"America/Santiago", "2026-09-06", "2026-09-06T04:00:00Z", "2026-09-07T03:00:00Z", 23},
		{"America/Havana", "2026-03-08", "2026-03-08T05:00:00Z", "2026-03-09T04:00:00Z", 23},
		{"America/Toronto", "2026-03-08", "2026-03-08T05:00:00Z", "2026-03-09T04:00:00Z", 23},
		{"America/Toronto", "2026-11-01", "2026-11-01T04:00:00Z", "2026-11-02T05:00:00Z", 25},
		{"America/Toronto", "2026-07-01", "2026-07-01T04:00:00Z", "2026-07-02T04:00:00Z", 24},
		{"Australia/Lord_Howe", "2026-04-05", "2026-04-04T13:00:00Z", "2026-04-05T13:30:00Z", 0},
		{"Asia/Kathmandu", "2026-03-08", "2026-03-07T18:15:00Z", "2026-03-08T18:15:00Z", 24},
		{"UTC", "2026-03-08", "2026-03-08T00:00:00Z", "2026-03-09T00:00:00Z", 24},
	}
	for _, tt := range tests {
		t.Run(tt.zone+" "+tt.date, func(t *testing.T) {
			loc := mustZone(t, tt.zone)
			start, end, ok := DayBounds(mustDate(t, tt.date), loc)

			require.True(t, ok)
			assert.True(t, mustInstant(t, tt.start).Equal(start), "start %s", start.UTC())
			assert.True(t, mustInstant(t, tt.end).Equal(end), "end %s", end.UTC())
			assert.Equal(t, loc, start.Location())
			if tt.hours != 0 {
				assert.Equal(t, time.Duration(tt.hours)*time.Hour, end.Sub(start))
			}
		})
	}
}

func TestDayBoundsDateMissingFromZone(t *testing.T) {
	apia := mustZone(t, "Pacific/Apia")

	start, end, ok := DayBounds(mustDate(t, "2011-12-30"), apia)

	assert.False(t, ok)
	next := mustInstant(t, "2011-12-30T10:00:00Z")
	assert.True(t, next.Equal(start), "start %s", start.UTC())
	assert.True(t, next.Equal(end), "end %s", end.UTC())

	_, before, ok := DayBounds(mustDate(t, "2011-12-29"), apia)
	require.True(t, ok)
	assert.True(t, next.Equal(before), "the day before ends where the next existing day starts")
}

func TestDayBoundsInvalidDate(t *testing.T) {
	_, _, ok := DayBounds(Date{Year: 2026, Month: time.February, Day: 30}, time.UTC)

	assert.False(t, ok)
}

func TestDayBoundsNilLocationIsUTC(t *testing.T) {
	start, end, ok := DayBounds(mustDate(t, "2026-03-08"), nil)

	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC), start)
	assert.Equal(t, time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC), end)
}

// TestTimeDateMidnightBug documents why DayBounds exists: where a zone skips
// local midnight, time.Date normalizes 00:00 backward into the previous day,
// so a day built from it would start an hour early and on the wrong date.
func TestTimeDateMidnightBug(t *testing.T) {
	tests := []struct {
		zone, date, timeDate string
	}{
		{"America/Santiago", "2026-09-06", "2026-09-05T23:00:00-04:00"},
		{"America/Havana", "2026-03-08", "2026-03-07T23:00:00-05:00"},
	}
	for _, tt := range tests {
		t.Run(tt.zone, func(t *testing.T) {
			loc := mustZone(t, tt.zone)
			d := mustDate(t, tt.date)

			naive := time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, loc)
			start, _, ok := DayBounds(d, loc)

			require.True(t, ok)
			assert.True(t, mustInstant(t, tt.timeDate).Equal(naive), "time.Date gave %s", naive)
			assert.Equal(t, d.AddDays(-1), DateOf(naive, loc), "time.Date lands on the previous day")
			assert.Equal(t, d, DateOf(start, loc))
			assert.Equal(t, time.Hour, start.Sub(naive))
		})
	}
}

func TestDateOf(t *testing.T) {
	santiago := mustZone(t, "America/Santiago")

	assert.Equal(t, mustDate(t, "2026-09-05"), DateOf(mustInstant(t, "2026-09-06T03:30:00Z"), santiago))
	assert.Equal(t, mustDate(t, "2026-09-06"), DateOf(mustInstant(t, "2026-09-06T04:00:00Z"), santiago))
	assert.Equal(t, mustDate(t, "2026-09-06"), DateOf(mustInstant(t, "2026-09-06T03:30:00Z"), nil))
}

func TestResolveLocal(t *testing.T) {
	tests := []struct {
		name, zone, wall, want string
		res                    Resolution
	}{
		{"gap takes the first instant after it", "America/Toronto", "2026-03-08T02:30", "2026-03-08T07:00:00Z", Gap},
		{"gap start", "America/Toronto", "2026-03-08T02:00", "2026-03-08T07:00:00Z", Gap},
		{"gap last nanosecond", "America/Toronto", "2026-03-08T02:59:59.999999999", "2026-03-08T07:00:00Z", Gap},
		{"just after the gap", "America/Toronto", "2026-03-08T03:00", "2026-03-08T07:00:00Z", Unique},
		{"just before the gap", "America/Toronto", "2026-03-08T01:59:59", "2026-03-08T06:59:59Z", Unique},
		{"overlap takes the earlier instant", "America/Toronto", "2026-11-01T01:30", "2026-11-01T05:30:00Z", Overlap},
		{"overlap start", "America/Toronto", "2026-11-01T01:00", "2026-11-01T05:00:00Z", Overlap},
		{"after the overlap", "America/Toronto", "2026-11-01T02:00", "2026-11-01T07:00:00Z", Unique},
		{"skipped midnight", "America/Santiago", "2026-09-06T00:00", "2026-09-06T04:00:00Z", Gap},
		{"half-hour gap", "Australia/Lord_Howe", "2026-10-04T02:15", "2026-10-03T15:30:00Z", Gap},
		{"half-hour overlap", "Australia/Lord_Howe", "2026-04-05T01:45", "2026-04-04T14:45:00Z", Overlap},
		{"skipped day", "Pacific/Apia", "2011-12-30T12:00", "2011-12-30T10:00:00Z", Gap},
		{"fraction kept", "UTC", "2026-03-08T02:30:00.25", "2026-03-08T02:30:00.25Z", Unique},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc := mustZone(t, tt.zone)

			at, res := ResolveLocal(mustDateTime(t, tt.wall), loc)

			assert.True(t, mustInstant(t, tt.want).Equal(at), "got %s", at.UTC())
			assert.Equal(t, tt.res, res)
			assert.Equal(t, loc, at.Location())
		})
	}
}

func TestResolveLocalDiffersFromTimeDateInGap(t *testing.T) {
	toronto := mustZone(t, "America/Toronto")

	at, _ := ResolveLocal(mustDateTime(t, "2026-03-08T02:30"), toronto)
	naive := time.Date(2026, time.March, 8, 2, 30, 0, 0, toronto)

	assert.True(t, mustInstant(t, "2026-03-08T07:00:00Z").Equal(at))
	assert.False(t, naive.Equal(at), "time.Date gave %s", naive.UTC())
}

func TestLoadZone(t *testing.T) {
	for _, name := range []string{"UTC", "America/Toronto", "Etc/GMT+5", "Etc/UTC", "Pacific/Apia", "US/Central"} {
		t.Run("accepts "+name, func(t *testing.T) {
			loc, err := LoadZone(name)

			require.NoError(t, err)
			assert.Equal(t, name, loc.String())
		})
	}
	for _, name := range []string{
		"", "Local", "local", "utc", " UTC", "EST", "CET", "GMT", "EST5EDT", "+02:00", "-0500", "Z",
		"America/Atlantis", "America/Toronto ", "/etc/localtime", "../America/Toronto", "America/../Europe/Paris",
		"posix/America/Toronto", "right/UTC",
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := LoadZone(name)

			assert.ErrorIs(t, err, ErrInvalidZone)
		})
	}
}

func TestParseDate(t *testing.T) {
	valid := map[string]Date{
		"2026-03-08": {2026, time.March, 8},
		"2024-02-29": {2024, time.February, 29},
		"0000-01-01": {0, time.January, 1},
		"9999-12-31": {9999, time.December, 31},
	}
	for s, want := range valid {
		t.Run(s, func(t *testing.T) {
			got, err := ParseDate(s)

			require.NoError(t, err)
			assert.Equal(t, want, got)
			assert.Equal(t, s, got.String())
		})
	}
	for _, s := range []string{
		"", "2026-3-8", "2026-03-8", "26-03-08", "2026/03/08", "2026-02-29", "2026-02-30", "2026-13-01",
		"2026-00-10", "2026-01-00", "2026-01-32", "+026-01-01", "2026-03-08T00:00", " 2026-03-08", "２026-03-08",
	} {
		t.Run("rejects "+s, func(t *testing.T) {
			_, err := ParseDate(s)

			assert.ErrorIs(t, err, ErrInvalidDate)
		})
	}
}

func TestDateArithmetic(t *testing.T) {
	d := mustDate(t, "2024-02-28")

	assert.Equal(t, mustDate(t, "2024-02-29"), d.AddDays(1))
	assert.Equal(t, mustDate(t, "2024-03-01"), d.AddDays(2))
	assert.Equal(t, mustDate(t, "2023-12-31"), d.AddDays(-59))
	assert.Equal(t, mustDate(t, "2025-02-27"), d.AddDays(365))
	assert.Equal(t, d, d.AddDays(0))

	assert.Equal(t, 0, d.Compare(mustDate(t, "2024-02-28")))
	assert.Equal(t, -1, d.Compare(mustDate(t, "2024-03-01")))
	assert.Equal(t, 1, d.Compare(mustDate(t, "2024-02-01")))
	assert.Equal(t, 1, d.Compare(mustDate(t, "2023-12-31")))
	assert.Equal(t, -1, d.Compare(mustDate(t, "2025-01-01")))
}

func TestParseDateTime(t *testing.T) {
	tests := []struct {
		in, canonical string
		want          DateTime
	}{
		{"2026-03-08T02:30", "2026-03-08T02:30:00", DateTime{Date{2026, 3, 8}, 2, 30, 0, 0}},
		{"2026-03-08T02:30:15", "2026-03-08T02:30:15", DateTime{Date{2026, 3, 8}, 2, 30, 15, 0}},
		{"2026-03-08T23:59:59.5", "2026-03-08T23:59:59.5", DateTime{Date{2026, 3, 8}, 23, 59, 59, 500_000_000}},
		{"2026-03-08T00:00:00.000000001", "2026-03-08T00:00:00.000000001", DateTime{Date{2026, 3, 8}, 0, 0, 0, 1}},
		{"2026-03-08T00:00:00.120", "2026-03-08T00:00:00.12", DateTime{Date{2026, 3, 8}, 0, 0, 0, 120_000_000}},
		{"2026-03-08T00:00:00.000", "2026-03-08T00:00:00", DateTime{Date{2026, 3, 8}, 0, 0, 0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseDateTime(tt.in)

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.canonical, got.String())
		})
	}
	for _, s := range []string{
		"", "2026-03-08", "2026-03-08T", "2026-03-08T2:30", "2026-03-08 02:30", "2026-03-08t02:30",
		"2026-03-08T24:00", "2026-03-08T12:60", "2026-03-08T12:00:60", "2026-03-08T12:00:0",
		"2026-03-08T12:00:00.", "2026-03-08T12:00:00.1234567890", "2026-03-08T12:00:00,5",
		"2026-03-08T12:00Z", "2026-03-08T12:00:00Z", "2026-03-08T12:00:00+01:00", "2026-03-08T12:00:00-0500",
		"2026-03-08T12:00:00.5Z", "2026-02-30T12:00", "2026-03-08T12:00:00 UTC",
	} {
		t.Run("rejects "+s, func(t *testing.T) {
			_, err := ParseDateTime(s)

			assert.ErrorIs(t, err, ErrInvalidDate)
		})
	}
}

func TestRangeBounds(t *testing.T) {
	toronto := mustZone(t, "America/Toronto")

	start, end, err := RangeBounds(mustDate(t, "2026-03-07"), mustDate(t, "2026-03-08"), toronto)

	require.NoError(t, err)
	assert.True(t, mustInstant(t, "2026-03-07T05:00:00Z").Equal(start))
	assert.True(t, mustInstant(t, "2026-03-09T04:00:00Z").Equal(end))

	single, singleEnd, err := RangeBounds(mustDate(t, "2026-11-01"), mustDate(t, "2026-11-01"), toronto)
	require.NoError(t, err)
	dayStart, dayEnd, ok := DayBounds(mustDate(t, "2026-11-01"), toronto)
	require.True(t, ok)
	assert.Equal(t, dayStart, single)
	assert.Equal(t, dayEnd, singleEnd)
}

func TestRangeBoundsSkipsMissingDateInside(t *testing.T) {
	apia := mustZone(t, "Pacific/Apia")

	start, end, err := RangeBounds(mustDate(t, "2011-12-29"), mustDate(t, "2011-12-31"), apia)

	require.NoError(t, err)
	assert.True(t, mustInstant(t, "2011-12-29T10:00:00Z").Equal(start), "start %s", start.UTC())
	assert.True(t, mustInstant(t, "2011-12-31T10:00:00Z").Equal(end), "end %s", end.UTC())
}

func TestRangeBoundsErrors(t *testing.T) {
	apia := mustZone(t, "Pacific/Apia")
	missing := mustDate(t, "2011-12-30")

	_, _, err := RangeBounds(missing, mustDate(t, "2011-12-31"), apia)
	require.ErrorIs(t, err, ErrDateNotInZone)

	_, _, err = RangeBounds(mustDate(t, "2011-12-29"), missing, apia)
	require.ErrorIs(t, err, ErrDateNotInZone)

	_, _, err = RangeBounds(mustDate(t, "2026-03-09"), mustDate(t, "2026-03-08"), apia)
	require.ErrorIs(t, err, ErrInvalidDate)

	_, _, err = RangeBounds(Date{2026, time.February, 30}, mustDate(t, "2026-03-08"), apia)
	require.ErrorIs(t, err, ErrInvalidDate)
}

func TestClassifyZone(t *testing.T) {
	type want struct {
		state  ZoneState
		offset int
	}
	tests := []struct {
		text           string
		general, email want
	}{
		{"", want{ZoneOmitted, 0}, want{ZoneOmitted, 0}},
		{"   ", want{ZoneOmitted, 0}, want{ZoneOmitted, 0}},
		{"Z", want{ZoneUTC, 0}, want{ZoneOmitted, 0}},
		{"z", want{ZoneUTC, 0}, want{ZoneOmitted, 0}},
		{"UTC", want{ZoneUTC, 0}, want{ZoneUTC, 0}},
		{"utc", want{ZoneUTC, 0}, want{ZoneUTC, 0}},
		{"+00:00", want{ZoneUTC, 0}, want{ZoneUTC, 0}},
		{"+0000", want{ZoneUTC, 0}, want{ZoneUTC, 0}},
		{"-0000", want{ZoneUTC, 0}, want{ZoneOmitted, 0}},
		{"-00:00", want{ZoneUTC, 0}, want{ZoneOmitted, 0}},
		{"-04:00", want{ZoneOffset, -4 * 3600}, want{ZoneOffset, -4 * 3600}},
		{"+0530", want{ZoneOffset, 5*3600 + 30*60}, want{ZoneOffset, 5*3600 + 30*60}},
		{"+18:00", want{ZoneOffset, 18 * 3600}, want{ZoneOffset, 18 * 3600}},
		{"-1800", want{ZoneOffset, -18 * 3600}, want{ZoneOffset, -18 * 3600}},
		{"+18:01", want{ZoneInvalid, 0}, want{ZoneInvalid, 0}},
		{"+25:00", want{ZoneInvalid, 0}, want{ZoneInvalid, 0}},
		{"+4", want{ZoneInvalid, 0}, want{ZoneInvalid, 0}},
		{"+04", want{ZoneInvalid, 0}, want{ZoneInvalid, 0}},
		{"+04:60", want{ZoneInvalid, 0}, want{ZoneInvalid, 0}},
		{"+04:3", want{ZoneInvalid, 0}, want{ZoneInvalid, 0}},
		{"America/New_York", want{ZoneIANA, 0}, want{ZoneNamed, 0}},
		{"America/Atlantis", want{ZoneInvalid, 0}, want{ZoneInvalid, 0}},
		{"UT", want{ZoneNamed, 0}, want{ZoneRFC5322, 0}},
		{"GMT", want{ZoneNamed, 0}, want{ZoneRFC5322, 0}},
		{"EST", want{ZoneNamed, 0}, want{ZoneRFC5322, -5 * 3600}},
		{"EDT", want{ZoneNamed, 0}, want{ZoneRFC5322, -4 * 3600}},
		{"CST", want{ZoneNamed, 0}, want{ZoneRFC5322, -6 * 3600}},
		{"CDT", want{ZoneNamed, 0}, want{ZoneRFC5322, -5 * 3600}},
		{"MST", want{ZoneNamed, 0}, want{ZoneRFC5322, -7 * 3600}},
		{"MDT", want{ZoneNamed, 0}, want{ZoneRFC5322, -6 * 3600}},
		{"PST", want{ZoneNamed, 0}, want{ZoneRFC5322, -8 * 3600}},
		{"pdt", want{ZoneNamed, 0}, want{ZoneRFC5322, -7 * 3600}},
		{"CET", want{ZoneNamed, 0}, want{ZoneNamed, 0}},
		{"IST", want{ZoneNamed, 0}, want{ZoneNamed, 0}},
		{"BST", want{ZoneNamed, 0}, want{ZoneNamed, 0}},
		{"AEST", want{ZoneNamed, 0}, want{ZoneNamed, 0}},
		{"A", want{ZoneNamed, 0}, want{ZoneOmitted, 0}},
		{"J", want{ZoneNamed, 0}, want{ZoneNamed, 0}},
		{"EST5EDT", want{ZoneInvalid, 0}, want{ZoneInvalid, 0}},
		{"Ünï", want{ZoneInvalid, 0}, want{ZoneInvalid, 0}},
		{"(UTC)", want{ZoneInvalid, 0}, want{ZoneInvalid, 0}},
	}
	for _, tt := range tests {
		for _, mode := range []Mode{General, Email} {
			w := tt.general
			if mode == Email {
				w = tt.email
			}
			t.Run(tt.text+" "+modeName(mode), func(t *testing.T) {
				d := ClassifyZone(tt.text, mode)

				assert.Equal(t, w.state, d.State)
				assert.Equal(t, w.offset, d.OffsetSeconds)
				assert.Equal(t, tt.text, d.Text)
				assert.Equal(t, d.State == ZoneIANA, d.Location != nil)
			})
		}
	}
}

func modeName(m Mode) string {
	if m == Email {
		return "email"
	}
	return "general"
}

func TestZoneStateExact(t *testing.T) {
	exact := map[ZoneState]bool{
		ZoneUTC: true, ZoneOffset: true, ZoneIANA: true, ZoneRFC5322: true,
		ZoneNamed: false, ZoneOmitted: false, ZoneInvalid: false, "": false,
	}
	for state, want := range exact {
		assert.Equal(t, want, state.Exact(), "state %q", state)
	}
}

func TestParsePrecision(t *testing.T) {
	for _, p := range []Precision{
		PrecisionYear, PrecisionMonth, PrecisionDate, PrecisionHour,
		PrecisionMinute, PrecisionSecond, PrecisionFraction,
	} {
		got, err := ParsePrecision(string(p))
		require.NoError(t, err)
		assert.Equal(t, p, got)
	}
	_, err := ParsePrecision("day")
	require.Error(t, err)
	_, err = ParsePrecision("Second")
	require.Error(t, err)
}

func TestResolve(t *testing.T) {
	wall := mustDateTime(t, "2026-03-08T02:30")
	tests := []struct {
		text  string
		mode  Mode
		want  string
		exact bool
		res   Resolution
	}{
		{"Z", General, "2026-03-08T02:30:00Z", true, Unique},
		{"-04:00", General, "2026-03-08T06:30:00Z", true, Unique},
		{"+0545", Email, "2026-03-07T20:45:00Z", true, Unique},
		{"EST", Email, "2026-03-08T07:30:00Z", true, Unique},
		{"America/Toronto", General, "2026-03-08T07:00:00Z", true, Gap},
		{"EST", General, "", false, Unique},
		{"America/Toronto", Email, "", false, Unique},
		{"-0000", Email, "", false, Unique},
		{"", General, "", false, Unique},
		{"+25:00", General, "", false, Unique},
	}
	for _, tt := range tests {
		t.Run(tt.text+" "+modeName(tt.mode), func(t *testing.T) {
			at, exact, res := Resolve(wall, ClassifyZone(tt.text, tt.mode))

			assert.Equal(t, tt.exact, exact)
			assert.Equal(t, tt.res, res)
			if !tt.exact {
				assert.True(t, at.IsZero())
				return
			}
			assert.True(t, mustInstant(t, tt.want).Equal(at), "got %s", at)
			assert.Equal(t, time.UTC, at.Location())
		})
	}
}

func TestResolveIANAWithoutLocationIsFloating(t *testing.T) {
	_, exact, _ := Resolve(mustDateTime(t, "2026-03-08T02:30"), Designator{State: ZoneIANA, Text: "America/Toronto"})

	assert.False(t, exact)
}

func TestResolutionString(t *testing.T) {
	assert.Equal(t, "unique", Unique.String())
	assert.Equal(t, "overlap", Overlap.String())
	assert.Equal(t, "gap", Gap.String())
	assert.Equal(t, "Resolution(7)", Resolution(7).String())
}
