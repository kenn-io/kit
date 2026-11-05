package backup

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/growthtest"
)

func TestAttachmentHistoryGrowth(t *testing.T) {
	// Allocs measures process-wide allocations, so run these checks sequentially.
	t.Run("prune", func(t *testing.T) {
		growthtest.Run(t, growthtest.Check{
			Small: 100, Large: 1000,
			Build: func(tb testing.TB, size int) (run, reset func()) {
				tb.Helper()
				r, _, _ := buildAttachmentHistory(tb, 0, size)
				return func() {
					_, err := Prune(tb.Context(), r, newTestApp(), PruneOptions{})
					require.NoError(tb, err)
				}, nil
			},
			Measure: growthtest.Allocs(3),
			// Each snapshot visits its growing attachment union, totaling
			// about N²/2 refs. Combined with linear work, this measures 33x
			// growth from 100 to 1,000 snapshots; allow a 1.5x margin.
			MaxRatio: 50,
			Floor:    10_000,
		})
	})
	t.Run("create", func(t *testing.T) {
		const large = 1000
		growthtest.Run(t, growthtest.Check{
			Small: 100, Large: large,
			Build: func(tb testing.TB, size int) (run, reset func()) {
				tb.Helper()
				r, opts, add := buildAttachmentHistory(tb, large-size, size)
				return func() {
					_, err := Create(tb.Context(), r, newTestApp(), opts)
					require.NoError(tb, err)
				}, add
			},
			Measure: growthtest.Allocs(3),
			// Equal live attachment counts and capped list segments should
			// keep growth flat; allow a 2x margin over that expectation.
			MaxRatio: 2,
			Floor:    10_000,
		})
	})
}
