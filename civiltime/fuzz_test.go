package civiltime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FuzzParseDate checks that every accepted date is a real calendar date that
// formats back to the exact input, and that AddDays and Compare agree.
func FuzzParseDate(f *testing.F) {
	for _, s := range []string{"2026-03-08", "2024-02-29", "2026-02-29", "0000-01-01", "9999-12-31", "2026-3-8", ""} {
		f.Add(s, 1)
	}
	f.Fuzz(func(t *testing.T, s string, n int) {
		d, err := ParseDate(s)
		if err != nil {
			require.ErrorIs(t, err, ErrInvalidDate)
			return
		}
		require.Equal(t, s, d.String())
		require.True(t, d.valid())

		n %= 100_000
		moved := d.AddDays(n)
		require.True(t, moved.valid())
		require.Equal(t, d, moved.AddDays(-n))
		switch {
		case n > 0:
			require.Equal(t, -1, d.Compare(moved))
		case n < 0:
			require.Equal(t, 1, d.Compare(moved))
		default:
			require.Equal(t, 0, d.Compare(moved))
		}
	})
}

// FuzzParseDateTime checks that formatting an accepted wall clock and
// parsing it again gives the same value, and that no accepted text carries
// an offset.
func FuzzParseDateTime(f *testing.F) {
	for _, s := range []string{
		"2026-03-08T02:30", "2026-03-08T02:30:15", "2026-03-08T02:30:15.120", "2026-03-08T02:30:15.000000001",
		"2026-03-08T02:30Z", "2026-03-08T02:30:00+01:00", "2026-03-08T24:00",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		dt, err := ParseDateTime(s)
		if err != nil {
			require.ErrorIs(t, err, ErrInvalidDate)
			return
		}
		again, err := ParseDateTime(dt.String())
		require.NoError(t, err)
		require.Equal(t, dt, again)
		require.NotContains(t, s, "Z")
		require.NotContains(t, s[10:], "+")
		require.NotContains(t, s[10:], "-")
	})
}

// FuzzClassifyZone checks that ClassifyZone never panics and returns a
// consistent designator in both modes, and that Resolve accepts it.
func FuzzClassifyZone(f *testing.F) {
	for _, s := range []string{
		"", "Z", "UTC", "+0000", "-0000", "-04:00", "+25:00", "+4", "America/New_York", "America/Atlantis",
		"EST", "CET", "A", "EST5EDT", "../etc", "/etc/localtime", "posix/UTC", " pst ", "\x00",
	} {
		f.Add(s)
	}
	wall := DateTime{Year: 2026, Month: time.March, Day: 8, Hour: 2, Minute: 30}
	f.Fuzz(func(t *testing.T, text string) {
		for _, mode := range []Mode{General, Email} {
			d := ClassifyZone(text, mode)

			assert.Equal(t, text, d.Text)
			assert.Contains(t, []ZoneState{ZoneUTC, ZoneOffset, ZoneIANA, ZoneRFC5322, ZoneNamed, ZoneOmitted, ZoneInvalid}, d.State)
			assert.Equal(t, d.State == ZoneIANA, d.Location != nil)
			assert.LessOrEqual(t, d.OffsetSeconds, maxOffset)
			assert.GreaterOrEqual(t, d.OffsetSeconds, -maxOffset)
			if d.State != ZoneOffset && d.State != ZoneRFC5322 {
				assert.Zero(t, d.OffsetSeconds)
			}
			if mode == Email {
				assert.NotEqual(t, ZoneIANA, d.State)
			} else {
				assert.NotEqual(t, ZoneRFC5322, d.State)
			}

			at, exact, _ := Resolve(wall, d)
			assert.Equal(t, d.State.Exact(), exact)
			assert.Equal(t, exact, !at.IsZero())
		}
	})
}
