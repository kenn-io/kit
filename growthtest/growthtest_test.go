package growthtest_test

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/growthtest"
)

type recordingTB struct {
	testing.TB
	errors []string
	logs   []string
}

func (tb *recordingTB) Errorf(format string, args ...any) {
	tb.errors = append(tb.errors, fmt.Sprintf(format, args...))
}

func (tb *recordingTB) Logf(format string, args ...any) {
	tb.logs = append(tb.logs, fmt.Sprintf(format, args...))
}

func allocationFixture(_ testing.TB, size int) (run, reset func()) {
	items := make([]*[32]byte, size)
	return func() {
		for i := range items {
			items[i] = new([32]byte)
		}
	}, nil
}

func TestAllocsGrowth(t *testing.T) {
	for _, quadratic := range []bool{false, true} {
		t.Run(fmt.Sprintf("quadratic=%t", quadratic), func(t *testing.T) {
			tb := &recordingTB{TB: t}
			growthtest.Run(tb, growthtest.Check{
				Small: 1000, Large: 10000, MaxRatio: 20,
				Build: func(tb testing.TB, size int) (run, reset func()) {
					tb.Helper()
					if quadratic {
						size = size * size / 100
					}
					return allocationFixture(tb, size)
				},
				Measure: growthtest.Allocs(3),
			})
			if quadratic {
				require.Len(t, tb.errors, 1)
				assert.Contains(t, tb.errors[0], "small(size=1000)=10000 large(size=10000)=1e+06 ratio=100 bound=20")
			} else {
				assert.Empty(t, tb.errors)
				assert.Contains(t, strings.Join(tb.logs, "\n"), "small(size=1000)=1000 large(size=10000)=10000 ratio=10 bound=20")
			}
		})
	}
}

func TestFloor(t *testing.T) {
	for _, floor := range []float64{2000, 0} {
		t.Run(fmt.Sprintf("floor=%g", floor), func(t *testing.T) {
			tb := &recordingTB{TB: t}
			growthtest.Run(tb, growthtest.Check{
				Small: 1000, Large: 10000, MaxRatio: 20, Floor: floor,
				Build: allocationFixture, Measure: growthtest.Allocs(3),
			})
			if floor > 0 {
				require.Len(t, tb.errors, 1)
				assert.Contains(t, tb.errors[0], "Floor=2000")
				assert.Contains(t, tb.errors[0], "small(size=1000)=1000 large(size=10000)=10000 ratio=10 bound=20")
			} else {
				assert.Empty(t, tb.errors)
			}
		})
	}
}

var cpuSink uint64

func cpuFixture(_ testing.TB, size int) (run, reset func()) {
	return func() {
		x := cpuSink
		for range size {
			x = x*6364136223846793005 + 1
		}
		cpuSink = x
	}, nil
}

func TestFastestGrowth(t *testing.T) {
	for _, quadratic := range []bool{false, true} {
		t.Run(fmt.Sprintf("quadratic=%t", quadratic), func(t *testing.T) {
			tb := &recordingTB{TB: t}
			growthtest.Run(tb, growthtest.Check{
				Small: 1000, Large: 10000, MaxRatio: 30,
				Build: func(tb testing.TB, size int) (run, reset func()) {
					tb.Helper()
					if quadratic {
						return cpuFixture(tb, size*size)
					}
					return cpuFixture(tb, size*1000)
				},
				Measure: growthtest.Fastest(5),
			})
			if quadratic {
				require.Len(t, tb.errors, 1)
				assert.Regexp(t, `small\(size=1000\)=\S+ large\(size=10000\)=\S+ ratio=\S+ bound=30`, tb.errors[0])
			} else {
				assert.Empty(t, tb.errors)
			}
		})
	}
}

func TestConsumingFixtureReset(t *testing.T) {
	for _, name := range []string{"allocs", "fastest"} {
		t.Run(name, func(t *testing.T) {
			measure := growthtest.Allocs(3)
			if name == "fastest" {
				measure = growthtest.Fastest(5)
			}
			tb := &recordingTB{TB: t}
			growthtest.Run(tb, growthtest.Check{
				Small: 1000, Large: 10000, MaxRatio: 30, Measure: measure,
				Build: func(tb testing.TB, size int) (run, reset func()) {
					tb.Helper()
					var op func()
					if name == "allocs" {
						op, _ = allocationFixture(tb, size*size/100)
					} else {
						op, _ = cpuFixture(tb, size*size)
					}
					pending := true
					return func() {
						if pending {
							op()
							pending = false
						}
					}, func() { pending = true }
				},
			})
			require.Len(t, tb.errors, 1, "reset must restore work before every sample")
		})
	}
}

func TestAllocsExcludesReset(t *testing.T) {
	var items []*[32]byte
	resets := 0
	got := growthtest.Allocs(3)(func() {
		items[0] = new([32]byte)
	}, func() {
		resets++
		items = make([]*[32]byte, 1000)
		for i := range items {
			items[i] = new([32]byte)
		}
	})
	assert.InDelta(t, 1, got, 0)
	assert.Equal(t, 4, resets, "reset also prepares the warm-up run")
}

func TestShortSkipsBuild(t *testing.T) {
	wasShort := testing.Short()
	require.NoError(t, flag.Set("test.short", "true"))
	t.Cleanup(func() { require.NoError(t, flag.Set("test.short", strconv.FormatBool(wasShort))) })
	built := false
	skipped := false
	t.Run("growth", func(t *testing.T) {
		defer func() { skipped = t.Skipped() }()
		growthtest.Run(t, growthtest.Check{
			Build: func(testing.TB, int) (run, reset func()) {
				built = true
				return func() {}, nil
			},
		})
	})
	assert.True(t, skipped)
	assert.False(t, built)
}
