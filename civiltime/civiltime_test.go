package civiltime

import (
	"testing"
	"time"

	"cloud.google.com/go/civil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

func mustDate(t *testing.T, s string) civil.Date {
	t.Helper()
	d, err := civil.ParseDate(s)
	require.NoError(t, err)
	return d
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
		// Midnight skipped: the day starts at 01:00.
		{"America/Santiago", "2026-09-06", "2026-09-06T04:00:00Z", "2026-09-07T03:00:00Z", 23},
		{"America/Havana", "2026-03-08", "2026-03-08T05:00:00Z", "2026-03-09T04:00:00Z", 23},
		// Midnight repeated: the day starts at the first one.
		{"Asia/Jerusalem", "2000-10-06", "2000-10-05T21:00:00Z", "2000-10-06T22:00:00Z", 25},
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
	_, _, ok := DayBounds(civil.Date{Year: 2026, Month: time.February, Day: 30}, time.UTC)

	assert.False(t, ok)
}

func TestDayBoundsNilLocationIsUTC(t *testing.T) {
	start, end, ok := DayBounds(mustDate(t, "2026-03-08"), nil)

	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC), start)
	assert.Equal(t, time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC), end)
}
