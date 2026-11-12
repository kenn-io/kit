package civiltime

import (
	"testing"
	"time"

	"cloud.google.com/go/civil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sweepStep is the default brute-force sampling interval. sampleStep
// shrinks it for a zone whose offsets or transitions fall off that grid.
const sweepStep = 15 * time.Minute

// TestDayBoundsSweep checks DayBounds against a brute-force definition for
// every date in several years across zones that skip midnight (Santiago,
// Havana), repeat midnight (Jerusalem), shift at 02:00 (Toronto, London),
// skip a whole day (Apia), shift by 30 minutes (Lord Howe), keep odd offsets
// (Kathmandu, Chatham), or never shift (Tokyo, UTC).
func TestDayBoundsSweep(t *testing.T) {
	t.Parallel()
	zones := []string{
		"America/Santiago", "America/Havana", "Asia/Jerusalem", "America/Toronto",
		"Europe/London", "Pacific/Apia", "Australia/Lord_Howe", "Asia/Kathmandu",
		"Pacific/Chatham", "Antarctica/Casey", "Asia/Tokyo", "UTC",
	}
	periods := [][2]int{{2000, 2002}, {2010, 2012}, {2025, 2027}}
	for _, name := range zones {
		for _, years := range periods {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				sweepZone(t, mustZone(t, name), years[0], years[1])
			})
		}
	}
}

func sweepZone(t *testing.T, loc *time.Location, firstYear, lastYear int) {
	t.Helper()
	first := civil.Date{Year: firstYear, Month: time.January, Day: 1}
	last := civil.Date{Year: lastYear, Month: time.December, Day: 31}
	lo := first.In(time.UTC).Add(-48 * time.Hour)
	hi := last.In(time.UTC).Add(72 * time.Hour)
	step := sampleStep(t, loc, lo, hi)

	// Brute force: walk every sample in order. firstGE[D] is the first sample
	// whose local date is D or later; seen[D] says whether any sample showed
	// D itself.
	firstGE := map[civil.Date]time.Time{}
	seen := map[civil.Date]bool{}
	pending := first
	for at := lo; at.Before(hi); at = at.Add(step) {
		d := civil.DateOf(at.In(loc))
		seen[d] = true
		for !pending.After(d) && !pending.After(last.AddDays(1)) {
			firstGE[pending] = at
			pending = pending.AddDays(1)
		}
	}
	require.Equal(t, last.AddDays(2), pending, "brute force did not reach the end of the sweep")

	for d := first; !d.After(last); d = d.AddDays(1) {
		start, end, ok := DayBounds(d, loc)
		if !assert.Equal(t, seen[d], ok, "%s exists", d) ||
			!assert.True(t, firstGE[d].Equal(start), "%s start: got %s, want %s", d, start.UTC(), firstGE[d].UTC()) ||
			!assert.True(t, firstGE[d.AddDays(1)].Equal(end), "%s end: got %s, want %s", d, end.UTC(), firstGE[d.AddDays(1)].UTC()) {
			return
		}
	}
}

// sampleStep returns the largest step, at most sweepStep, that divides every
// offset of loc and every transition instant in [lo, hi). Sampling from a
// UTC midnight at that step lands on every transition and every local
// midnight, so the samples find the exact first instant of each local date.
func sampleStep(t *testing.T, loc *time.Location, lo, hi time.Time) time.Duration {
	t.Helper()
	step := int64(sweepStep / time.Second)
	for at := lo; at.Before(hi); {
		local := at.In(loc)
		_, offset := local.Zone()
		step = gcd(step, int64(offset))
		_, end := local.ZoneBounds()
		if end.IsZero() || !end.Before(hi) {
			break
		}
		step = gcd(step, end.Unix())
		at = end
	}
	require.GreaterOrEqual(t, step, int64(60), "zone %s needs a step under one minute", loc)
	return time.Duration(step) * time.Second
}

func gcd(a, b int64) int64 {
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
