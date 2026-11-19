package backup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/growthtest"
)

func TestAttachmentHistoryGrowth(t *testing.T) {
	if testing.Short() {
		t.Skip("growth checks skip under -short")
	}
	// Allocs measures process-wide allocations, so run these checks sequentially.
	const large = 1000
	largeRepo, largeOpts, addLarge := buildAttachmentHistory(t, 0, large)
	t.Run("prune", func(t *testing.T) {
		growthtest.Run(t, growthtest.Check{
			Small: 100, Large: large,
			Build: func(tb testing.TB, size int) (run, reset func()) {
				tb.Helper()
				var r *Repo
				if size == large {
					// Copy before pruning so create measures the original index layout.
					root := filepath.Join(tb.TempDir(), "repo")
					require.NoError(tb, os.CopyFS(root, os.DirFS(largeRepo.Root())))
					var err error
					r, err = Open(root)
					require.NoError(tb, err)
				} else {
					r, _, _ = buildAttachmentHistory(tb, 0, size)
				}
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
		growthtest.Run(t, growthtest.Check{
			Small: 100, Large: large,
			Build: func(tb testing.TB, size int) (run, reset func()) {
				tb.Helper()
				r, opts, add := largeRepo, largeOpts, addLarge
				if size != large {
					r, opts, add = buildAttachmentHistory(tb, large-size, size)
				}
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
