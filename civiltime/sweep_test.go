package civiltime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sweepStep is the default brute-force sampling interval. sampleStep
// shrinks it for a zone whose offsets or transitions fall off that grid.
const sweepStep = 15 * time.Minute

// TestDayBoundsSweep checks DayBounds against a brute-force definition for
// every date in several years across zones that skip midnight (Santiago,
// Havana), shift at 02:00 (Toronto, London), skip a whole day (Apia), shift by
// 30 minutes (Lord Howe), keep odd offsets (Kathmandu, Chatham, St Johns),
// or never shift (Tokyo, UTC).
func TestDayBoundsSweep(t *testing.T) {
	t.Parallel()
	zones := []string{
		"America/Santiago", "America/Havana", "America/Toronto", "Europe/London",
		"Pacific/Apia", "Australia/Lord_Howe", "Asia/Kathmandu", "Pacific/Chatham",
		"America/St_Johns", "America/Goose_Bay", "Antarctica/Casey", "Asia/Tokyo", "UTC",
	}
	periods := [][2]int{{2005, 2007}, {2010, 2012}, {2025, 2027}}
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
	first := Date{Year: firstYear, Month: time.January, Day: 1}
	last := Date{Year: lastYear, Month: time.December, Day: 31}
	lo := first.midnight().Add(-48 * time.Hour)
	hi := last.midnight().Add(72 * time.Hour)
	step := sampleStep(t, loc, lo, hi)

	// Brute force: walk every sample in order. firstAtOrAfter[D] is the first
	// sample whose local date is D or later; seen[D] says whether any sample
	// showed D itself.
	firstGE := map[Date]time.Time{}
	seen := map[Date]bool{}
	pending := first
	for at := lo; at.Before(hi); at = at.Add(step) {
		d := DateOf(at, loc)
		seen[d] = true
		for pending.Compare(d) <= 0 && pending.Compare(last.AddDays(1)) <= 0 {
			firstGE[pending] = at
			pending = pending.AddDays(1)
		}
	}
	require.Equal(t, last.AddDays(2), pending, "brute force did not reach the end of the sweep")

	type bounds struct {
		start, end time.Time
		ok         bool
	}
	got := map[Date]bounds{}
	for d := first; d.Compare(last) <= 0; d = d.AddDays(1) {
		start, end, ok := DayBounds(d, loc)
		got[d] = bounds{start, end, ok}

		if !assert.Equal(t, seen[d], ok, "%s exists", d) ||
			!assert.True(t, firstGE[d].Equal(start), "%s start: got %s, want %s", d, start.UTC(), firstGE[d].UTC()) ||
			!assert.True(t, firstGE[d.AddDays(1)].Equal(end), "%s end: got %s, want %s", d, end.UTC(), firstGE[d.AddDays(1)].UTC()) {
			return
		}
	}

	// The days partition time with no overlap and no hole, and DayOf names
	// the day containing each sample. That is the date DateOf reports,
	// except where a zone set its clock back across midnight (St Johns and
	// Goose Bay until 2010-2011) and showed an earlier date again; there it
	// is the latest date already shown.
	latest := DateOf(first.midnight(), loc)
	for at := first.midnight().Add(24 * time.Hour); at.Before(last.midnight()); at = at.Add(step) {
		d := DateOf(at, loc)
		if d.Compare(latest) > 0 {
			latest = d
		}
		day := DayOf(at, loc)
		b := got[day]
		if !assert.Equal(t, latest, day, "DayOf(%s)", at.UTC()) ||
			!assert.True(t, b.ok && !at.Before(b.start) && at.Before(b.end), "%s outside the day of %s", at.UTC(), day) {
			return
		}
	}
}

// sampleStep returns the largest step, at most sweepStep, that divides every
// offset of loc and every transition instant in [lo, hi). Sampling from a
// UTC midnight at that step lands on every transition and every local
// midnight, so the samples find the exact first instant of each local date.
// America/St_Johns, which changed clocks at 00:01 until 2011, needs one
// minute.
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

// TestResolveLocalSweep checks ResolveLocal against the instants that show
// each wall clock: every quarter hour of the swept days resolves to the
// earliest instant showing it, or to the end of the gap that hides it.
func TestResolveLocalSweep(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"America/Santiago", "America/Toronto", "Australia/Lord_Howe", "Pacific/Apia", "Asia/Kathmandu"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			loc := mustZone(t, name)
			lo := time.Date(2011, time.January, 1, 0, 0, 0, 0, time.UTC)
			hi := time.Date(2012, time.January, 3, 0, 0, 0, 0, time.UTC)
			if name != "Pacific/Apia" {
				lo = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
				hi = time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)
			}

			// Map every sampled wall clock to the instants that show it.
			shown := map[time.Time][]time.Time{}
			for at := lo.Add(-48 * time.Hour); at.Before(hi.Add(48 * time.Hour)); at = at.Add(sweepStep) {
				l := at.In(loc)
				wall := time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), 0, 0, time.UTC)
				shown[wall] = append(shown[wall], at)
			}

			for wall := lo; wall.Before(hi); wall = wall.Add(sweepStep) {
				dt := DateTime{Date: dateOf(wall), Hour: wall.Hour(), Minute: wall.Minute()}
				at, res := ResolveLocal(dt, loc)
				instants := shown[wall]
				switch len(instants) {
				case 0:
					if !assert.Equal(t, Gap, res, "%s", dt) {
						return
					}
					// The answer is the first instant after the gap: its wall
					// clock is later than dt and the instant before it shows
					// an earlier wall clock.
					after := wallOf(at, loc)
					before := wallOf(at.Add(-time.Nanosecond), loc)
					if !assert.True(t, after.After(wall) && before.Before(wall), "%s resolved to %s", dt, at) {
						return
					}
				case 1:
					if !assert.Equal(t, Unique, res, "%s", dt) || !assert.True(t, instants[0].Equal(at), "%s", dt) {
						return
					}
				default:
					if !assert.Equal(t, Overlap, res, "%s", dt) || !assert.True(t, instants[0].Equal(at), "%s", dt) {
						return
					}
				}
			}
		})
	}
}

func wallOf(at time.Time, loc *time.Location) time.Time {
	l := at.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), l.Second(), l.Nanosecond(), time.UTC)
}
