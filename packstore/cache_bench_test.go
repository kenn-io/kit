package packstore

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func BenchmarkStoreReaderRotation(b *testing.B) {
	const readsPerOp = 1024
	for _, n := range []int{maxOpenReaders / 4, maxOpenReaders/4 + 1, maxOpenReaders / 2, maxOpenReaders, maxOpenReaders + 1} {
		b.Run(fmt.Sprintf("packs=%d", n), func(b *testing.B) {
			store, packs := buildReaderRotationPacks(b, n, 1000)
			for _, p := range packs {
				_, _, err := store.ReadBounded(b.Context(), p.Hash, 1<<20)
				require.NoError(b, err)
			}
			misses := 0
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				// Average random eviction over a batch even with CI's five operations.
				for j := range readsPerOp {
					p := packs[(i*readsPerOp+j)%n]
					store.mu.Lock()
					_, hit := store.packReaders[p.PackID]
					store.mu.Unlock()
					if !hit {
						misses++
					}
					_, _, err := store.ReadBounded(b.Context(), p.Hash, 1<<20)
					require.NoError(b, err)
				}
			}
			b.ReportMetric(float64(misses)/float64(b.N*readsPerOp), "miss/read")
		})
	}
}

// buildReaderRotationPacks writes one target blob and unique fillers per pack.
func buildReaderRotationPacks(b *testing.B, nPacks, entries int) (*Store, []IndexEntry) {
	b.Helper()
	layout, err := NewLayout(b.TempDir(), LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(b, err)
	locations := map[Hash]Location{}
	var packs []IndexEntry
	for p := range nPacks {
		w, err := pack.NewWriter(b.TempDir(), pack.WriterOptions{})
		require.NoError(b, err)
		var target pack.Entry
		for e := range entries {
			buf := make([]byte, 24)
			binary.BigEndian.PutUint64(buf, uint64(p))
			binary.BigEndian.PutUint64(buf[8:], uint64(e))
			copy(buf[16:], "rotation")
			entry, err := w.Append(buf)
			require.NoError(b, err)
			if e == 0 {
				target = entry
			}
		}
		id := w.ID()
		require.NoError(b, os.MkdirAll(filepath.Dir(layout.PackPath(id)), 0o700))
		_, err = w.Seal(layout.PackPath(id))
		require.NoError(b, err)
		indexed, err := indexEntryFromPack(target, id)
		require.NoError(b, err)
		locations[indexed.Hash] = Location{Member: true, Pack: &indexed}
		packs = append(packs, indexed)
	}
	store, err := NewStore(&mapResolver{locations: locations}, layout, StoreOptions{})
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	return store, packs
}
