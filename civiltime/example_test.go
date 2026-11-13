package civiltime_test

import (
	"fmt"
	"time"

	"cloud.google.com/go/civil"

	"go.kenn.io/kit/civiltime"
)

// ExampleDayBounds shows a day that starts at 01:00 because the zone skips
// midnight, and a date the zone skipped altogether.
func ExampleDayBounds() {
	santiago, err := time.LoadLocation("America/Santiago")
	if err != nil {
		panic(err)
	}
	day := civil.Date{Year: 2026, Month: time.September, Day: 6}
	start, end, ok := civiltime.DayBounds(day, santiago)
	fmt.Println(start.Format(time.RFC3339), end.Format(time.RFC3339), end.Sub(start), ok)

	// time.Date normalizes the missing midnight back into the previous day.
	fmt.Println(time.Date(2026, time.September, 6, 0, 0, 0, 0, santiago).Format(time.RFC3339))

	apia, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		panic(err)
	}
	_, _, ok = civiltime.DayBounds(civil.Date{Year: 2011, Month: time.December, Day: 30}, apia)
	fmt.Println(ok)
	// Output:
	// 2026-09-06T01:00:00-03:00 2026-09-07T00:00:00-03:00 23h0m0s true
	// 2026-09-05T23:00:00-04:00
	// false
}
