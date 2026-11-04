// Package growthtest checks how an operation's cost grows between two sizes.
// MaxRatio is expected growth times a margin: linear growth from 1k to 10k
// with a 2x margin uses 20; flat growth with a 2x margin uses 2.
// Floor is the smallest small-side value the check trusts, in the measure's units.
// It must be positive.
package growthtest

import (
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Measure returns the cost of run. It calls reset, when set, before each run
// and excludes reset from the cost so operations can consume their fixtures.
type Measure func(run, reset func()) float64

// Allocs returns the fewest allocations over runs samples after one warm-up run.
// It excludes reset's allocations and temporarily sets GOMAXPROCS to 1.
// Tests using it must not run in parallel: allocation counts are process-wide.
// It panics if runs is less than 1.
func Allocs(runs int) Measure {
	if runs < 1 {
		panic("growthtest: Allocs requires positive runs")
	}
	return func(run, reset func()) float64 {
		defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
		if reset != nil {
			reset()
		}
		run()
		var stats runtime.MemStats
		fewest := ^uint64(0)
		for range runs {
			if reset != nil {
				reset()
			}
			runtime.ReadMemStats(&stats)
			before := stats.Mallocs
			run()
			runtime.ReadMemStats(&stats)
			fewest = min(fewest, stats.Mallocs-before)
		}
		return float64(fewest)
	}
}

// Fastest returns the minimum time in nanoseconds over runs samples after
// one untimed warm-up. Before warm-up and each sample, reset, when set,
// runs outside the timer. Samples call runtime.GC before timing.
// It panics if runs is less than 1.
func Fastest(runs int) Measure {
	if runs < 1 {
		panic("growthtest: Fastest requires positive runs")
	}
	return func(run, reset func()) float64 {
		if reset != nil {
			reset()
		}
		run()
		fastest := time.Duration(1<<63 - 1)
		for range runs {
			if reset != nil {
				reset()
			}
			runtime.GC()
			start := time.Now()
			run()
			fastest = min(fastest, time.Since(start))
		}
		return float64(fastest)
	}
}

// Check describes the fixtures, measure, and acceptable growth.
type Check struct {
	Small, Large int
	// Build prepares one size and returns its operation and optional reset.
	Build   func(tb testing.TB, size int) (run, reset func())
	Measure Measure
	// MaxRatio bounds large/small; Floor is the positive minimum trusted small value.
	MaxRatio, Floor float64
}

// Run builds and measures small, then large, and reports both costs and their
// ratio. It fails non-fatally when small is below Floor or growth exceeds
// MaxRatio. Under -short it skips before validating or building fixtures.
func Run(tb testing.TB, c Check) {
	tb.Helper()
	if testing.Short() {
		tb.Skip("growthtest: growth checks skip under -short")
	}
	require.Positive(tb, c.Small, "Small must be positive")
	require.Greater(tb, c.Large, c.Small, "Large must exceed Small")
	require.Positive(tb, c.MaxRatio, "MaxRatio must be positive")
	require.Positive(tb, c.Floor, "Floor must be positive")
	require.NotNil(tb, c.Build, "Build must be set")
	require.NotNil(tb, c.Measure, "Measure must be set")
	run, reset := c.Build(tb, c.Small)
	small := c.Measure(run, reset)
	run, reset = c.Build(tb, c.Large)
	large := c.Measure(run, reset)
	ratio := large / small
	const report = "small(size=%d)=%g large(size=%d)=%g ratio=%g bound=%g"
	tb.Logf(report, c.Small, small, c.Large, large, ratio, c.MaxRatio)
	assert.GreaterOrEqualf(tb, small, c.Floor, "small below Floor=%g; "+report, c.Floor, c.Small, small, c.Large, large, ratio, c.MaxRatio)
	assert.LessOrEqualf(tb, large, small*c.MaxRatio, report, c.Small, small, c.Large, large, ratio, c.MaxRatio)
}
