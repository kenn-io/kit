package packstore

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkStoreReaderRotation(b *testing.B) {
	const readsPerOp = 1024
	for _, n := range []int{maxOpenReaders, maxOpenReaders + 1} {
		b.Run(fmt.Sprintf("packs=%d", n), func(b *testing.B) {
			store, packs := buildReaderRotationPacks(b, n, 1000)
			for _, p := range packs {
				_, _, err := store.ReadBounded(b.Context(), p.Hash, 1<<20)
				require.NoError(b, err)
			}
			// Sample misses before timing so cache inspection adds no per-read cost.
			misses := 0
			for i := range readsPerOp {
				p := packs[i%n]
				store.mu.Lock()
				_, hit := store.packReaders[p.PackID]
				store.mu.Unlock()
				if !hit {
					misses++
				}
				_, _, err := store.ReadBounded(b.Context(), p.Hash, 1<<20)
				require.NoError(b, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				// Average random eviction over a batch even with CI's five operations.
				for j := range readsPerOp {
					p := packs[(i*readsPerOp+j)%n]
					_, _, err := store.ReadBounded(b.Context(), p.Hash, 1<<20)
					require.NoError(b, err)
				}
			}
			b.ReportMetric(float64(misses)/float64(readsPerOp), "miss/read")
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
		contents := make([][]byte, entries)
		for e := range entries {
			buf := make([]byte, 24)
			binary.BigEndian.PutUint64(buf, uint64(p))
			binary.BigEndian.PutUint64(buf[8:], uint64(e))
			copy(buf[16:], "rotation")
			contents[e] = buf
		}
		indexed := buildStoreBenchmarkPack(b, layout, contents)
		locations[indexed.Hash] = Location{Member: true, Pack: &indexed}
		packs = append(packs, indexed)
	}
	store, err := NewStore(&mapResolver{locations: locations}, layout, StoreOptions{})
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	return store, packs
}
