package civiltime_test

import (
	"fmt"
	"time"

	"go.kenn.io/kit/civiltime"
)

// ExampleDayBounds shows a day that starts at 01:00 because the zone skips
// midnight, and a date the zone skipped altogether.
func ExampleDayBounds() {
	santiago, err := civiltime.LoadZone("America/Santiago")
	if err != nil {
		panic(err)
	}
	day, err := civiltime.ParseDate("2026-09-06")
	if err != nil {
		panic(err)
	}
	start, end, ok := civiltime.DayBounds(day, santiago)
	fmt.Println(start.Format(time.RFC3339), end.Format(time.RFC3339), end.Sub(start), ok)

	// time.Date normalizes the missing midnight back into the previous day.
	fmt.Println(time.Date(2026, time.September, 6, 0, 0, 0, 0, santiago).Format(time.RFC3339))

	apia, err := civiltime.LoadZone("Pacific/Apia")
	if err != nil {
		panic(err)
	}
	_, _, ok = civiltime.DayBounds(civiltime.Date{Year: 2011, Month: time.December, Day: 30}, apia)
	fmt.Println(ok)
	// Output:
	// 2026-09-06T01:00:00-03:00 2026-09-07T00:00:00-03:00 23h0m0s true
	// 2026-09-05T23:00:00-04:00
	// false
}

// ExampleDateOf buckets an instant on the local date of the zone the caller
// asked for.
func ExampleDateOf() {
	santiago, err := civiltime.LoadZone("America/Santiago")
	if err != nil {
		panic(err)
	}
	at := time.Date(2026, time.September, 6, 3, 30, 0, 0, time.UTC)
	fmt.Println(civiltime.DateOf(at, santiago), civiltime.DateOf(at, time.UTC))
	// Output:
	// 2026-09-05 2026-09-06
}

// ExampleDayOf places an instant on its day. In 2010 St John's set its
// clocks back at 00:01, so 03:00Z showed 23:30 on the previous date after the
// next day had begun. DateOf reports the clock's date; DayOf reports the day
// whose DayBounds contain the instant.
func ExampleDayOf() {
	stJohns, err := civiltime.LoadZone("America/St_Johns")
	if err != nil {
		panic(err)
	}
	at := time.Date(2010, time.November, 7, 3, 0, 0, 0, time.UTC)
	fmt.Println(at.In(stJohns).Format("2006-01-02 15:04 MST"))
	fmt.Println(civiltime.DateOf(at, stJohns), civiltime.DayOf(at, stJohns))
	// Output:
	// 2010-11-06 23:30 NST
	// 2010-11-06 2010-11-07
}

// ExampleRangeBounds turns an inclusive range of dates into the instants a
// query should cover.
func ExampleRangeBounds() {
	toronto, err := civiltime.LoadZone("America/Toronto")
	if err != nil {
		panic(err)
	}
	from := civiltime.Date{Year: 2026, Month: time.October, Day: 31}
	start, end, err := civiltime.RangeBounds(from, from.AddDays(1), toronto)
	if err != nil {
		panic(err)
	}
	fmt.Println(start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), end.Sub(start))
	// Output:
	// 2026-10-31T04:00:00Z 2026-11-02T05:00:00Z 49h0m0s
}

// ExampleResolveLocal resolves wall clocks that a daylight-saving change
// skips or repeats.
func ExampleResolveLocal() {
	toronto, err := civiltime.LoadZone("America/Toronto")
	if err != nil {
		panic(err)
	}
	for _, s := range []string{"2026-03-08T02:30", "2026-11-01T01:30", "2026-07-01T12:00"} {
		wall, err := civiltime.ParseDateTime(s)
		if err != nil {
			panic(err)
		}
		at, res := civiltime.ResolveLocal(wall, toronto)
		fmt.Println(wall, at.UTC().Format(time.RFC3339), res)
	}
	// Output:
	// 2026-03-08T02:30:00 2026-03-08T07:00:00Z gap
	// 2026-11-01T01:30:00 2026-11-01T05:30:00Z overlap
	// 2026-07-01T12:00:00 2026-07-01T16:00:00Z unique
}

// ExampleClassifyZone reads the zone written on a timestamp. EST names a
// fixed offset in an email Date: header but is ambiguous anywhere else.
func ExampleClassifyZone() {
	wall, err := civiltime.ParseDateTime("2026-03-10T23:40:00")
	if err != nil {
		panic(err)
	}
	for _, c := range []struct {
		text string
		mode civiltime.Mode
	}{
		{"-04:00", civiltime.General},
		{"EST", civiltime.Email},
		{"EST", civiltime.General},
		{"-0000", civiltime.Email},
		{"America/New_York", civiltime.General},
	} {
		d := civiltime.ClassifyZone(c.text, c.mode)
		at, exact, _ := civiltime.Resolve(wall, d)
		if !exact {
			fmt.Printf("%-16s %-8s floating %s\n", c.text, d.State, wall)
			continue
		}
		fmt.Printf("%-16s %-8s %s\n", c.text, d.State, at.Format(time.RFC3339))
	}
	// Output:
	// -04:00           offset   2026-03-11T03:40:00Z
	// EST              rfc5322  2026-03-11T04:40:00Z
	// EST              named    floating 2026-03-10T23:40:00
	// -0000            omitted  floating 2026-03-10T23:40:00
	// America/New_York iana     2026-03-11T03:40:00Z
}
