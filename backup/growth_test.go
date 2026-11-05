package backup

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/growthtest"
)

func TestAttachmentHistoryGrowth(t *testing.T) {
	// Allocate with the parent so fixtures outlive both checks. Allocs measures
	// process-wide allocations, so these checks must run sequentially.
	type history struct {
		repo *Repo
		opts CreateOptions
		add  func()
	}
	histories := map[[2]int]history{}
	build := func(base, count int) history {
		key := [2]int{base, count}
		if h, ok := histories[key]; ok {
			return h
		}
		r, opts, add := buildAttachmentHistory(t, base, count)
		h := history{repo: r, opts: opts, add: add}
		histories[key] = h
		return h
	}
	t.Run("prune", func(t *testing.T) {
		growthtest.Run(t, growthtest.Check{
			Small: 100, Large: 1000,
			Build: func(tb testing.TB, size int) (run, reset func()) {
				tb.Helper()
				h := build(0, size)
				return func() {
					_, err := Prune(tb.Context(), h.repo, newTestApp(), PruneOptions{})
					require.NoError(tb, err)
				}, nil
			},
			Measure: growthtest.Allocs(3),
			// Snapshot union checks visit all refs: between 10x and 100x
			// growth, measured at 33x, with a 1.5x margin.
			MaxRatio: 50,
			Floor:    10_000,
		})
	})
	t.Run("create", func(t *testing.T) {
		growthtest.Run(t, growthtest.Check{
			Small: 100, Large: 1000,
			Build: func(tb testing.TB, size int) (run, reset func()) {
				tb.Helper()
				h := build(1000-size, size)
				return func() {
					_, err := Create(tb.Context(), h.repo, newTestApp(), h.opts)
					require.NoError(tb, err)
				}, h.add
			},
			Measure: growthtest.Allocs(3),
			// Equal live attachment counts and capped list segments should
			// keep growth flat; allow a 2x margin over that expectation.
			MaxRatio: 2,
			Floor:    10_000,
		})
	})
}
