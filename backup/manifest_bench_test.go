package backup

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/atomicfile"
)

func BenchmarkLatestSnapshot(b *testing.B) {
	for _, count := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("snapshots-%d", count), func(b *testing.B) {
			repo, err := Init(filepath.Join(b.TempDir(), "repo"))
			require.NoError(b, err)
			originalPublish := publishFile
			b.Cleanup(func() { publishFile = originalPublish })
			publishFile = func(path string, data []byte, opts ...atomicfile.Option) error {
				return originalPublish(path, data, append(opts, atomicfile.WithoutSync())...)
			}
			recipes := make([]string, 200)
			for i := range recipes {
				recipes[i] = blobID(fmt.Sprintf("recipe-%d", i)).String()
			}
			created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
			var latestID string
			for i := range count {
				m := testManifest(created.Add(time.Duration(i)*time.Second).Format(time.RFC3339), "", 0)
				m.FormatVersion = 5
				m.MinReaderVersion = 5
				m.Attachments.Recipes = recipes
				latestID, err = repo.WriteManifest(m)
				require.NoError(b, err)
			}
			b.ReportAllocs()
			for b.Loop() {
				m, err := repo.LatestSnapshot()
				require.NoError(b, err)
				require.Equal(b, latestID, m.SnapshotID)
			}
		})
	}
}
