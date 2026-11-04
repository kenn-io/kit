package packstore

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func BenchmarkStorePackedReads(b *testing.B) {
	content := bytes.Repeat([]byte("packstore stream benchmark "), (1<<20)/27)
	benchmarkStorePackedReads(b, content)
}

func BenchmarkStorePackedReadsRaw(b *testing.B) {
	content := make([]byte, 1<<20)
	reader := &maintenanceBenchNoiseReader{state: 1}
	_, err := io.ReadFull(reader, content)
	require.NoError(b, err)
	benchmarkStorePackedReads(b, content)
}

func BenchmarkStorePackedReadsLargeRaw(b *testing.B) {
	content := make([]byte, 4<<20)
	reader := &maintenanceBenchNoiseReader{state: 1}
	_, err := io.ReadFull(reader, content)
	require.NoError(b, err)
	benchmarkStorePackedReads(b, content)
}

func benchmarkStorePackedReads(b *testing.B, content []byte) {
	b.Helper()
	root := b.TempDir()
	layout, err := NewLayout(root, LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(b, err)
	indexed := buildStoreBenchmarkPack(b, layout, [][]byte{content})
	hash := indexed.Hash
	store, err := NewStore(&mapResolver{locations: map[Hash]Location{
		hash: {Member: true, Pack: &indexed},
	}}, layout, StoreOptions{})
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })

	for _, mode := range []string{"stream", "buffered"} {
		b.Run(mode, func(b *testing.B) {
			require := require.New(b)
			b.ReportAllocs()
			b.SetBytes(int64(len(content)))
			for range b.N {
				if mode == "stream" {
					reader, _, openErr := store.OpenStream(b.Context(), hash)
					require.NoError(openErr)
					_, copyErr := io.Copy(io.Discard, reader)
					require.NoError(errors.Join(copyErr, reader.Close()))
					continue
				}
				reader, _, openErr := store.Open(b.Context(), hash)
				require.NoError(openErr)
				_, copyErr := io.Copy(io.Discard, reader)
				require.NoError(copyErr)
				require.NoError(reader.Close())
			}
		})
	}
}

// buildStoreBenchmarkPack seals a pack and returns its first entry for reads.
func buildStoreBenchmarkPack(b *testing.B, layout Layout, contents [][]byte) IndexEntry {
	b.Helper()
	w, err := pack.NewWriter(b.TempDir(), pack.WriterOptions{})
	require.NoError(b, err)
	var target pack.Entry
	for i, content := range contents {
		entry, err := w.Append(content)
		require.NoError(b, err)
		if i == 0 {
			target = entry
		}
	}
	id := w.ID()
	_, err = w.Seal(layout.PackPath(id))
	require.NoError(b, err)
	indexed, err := indexEntryFromPack(target, id)
	require.NoError(b, err)
	return indexed
}

func BenchmarkMaintenancePackUnpack(b *testing.B) {
	const size = int64(1 << 20)
	for _, tt := range []struct {
		name   string
		source func() io.Reader
	}{
		{name: "raw-1MiB", source: func() io.Reader { return io.LimitReader(&maintenanceBenchNoiseReader{state: 1}, size) }},
		{name: "compressed-1MiB", source: func() io.Reader { return io.LimitReader(streamZeroReader{}, size) }},
	} {
		b.Run(tt.name, func(b *testing.B) {
			require := require.New(b)
			layout, err := NewLayout(b.TempDir(), LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
			require.NoError(err)
			loose, err := NewLooseStore(layout)
			require.NoError(err)
			written, err := loose.Write(b.Context(), tt.source(), WriteOptions{
				Durability: AtomicPublication, Dedup: VerifyFullHash, MaxBytes: size,
			})
			require.NoError(err)
			catalog := newMaintenanceCatalog()
			catalog.addLoose(written.Hash, written.Path)
			maintainer := newMaintainerForTest(b, catalog, layout, DefaultLimits())

			b.ReportAllocs()
			b.SetBytes(2 * size)
			b.ResetTimer()
			for range b.N {
				_, err := maintainer.Pack(b.Context(), PackOptions{})
				require.NoError(err)
				_, err = maintainer.Unpack(b.Context())
				require.NoError(err)
			}
		})
	}
}

func BenchmarkMaintenancePackIntoPackedStore(b *testing.B) {
	for _, tt := range []struct {
		name string
		size int64
	}{
		{name: "4MiB", size: 64 << 10},
		{name: "64MiB", size: 1 << 20},
		{name: "256MiB", size: 4 << 20},
	} {
		b.Run(tt.name, func(b *testing.B) {
			require := require.New(b)
			layout, err := NewLayout(b.TempDir(), LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
			require.NoError(err)
			loose, err := NewLooseStore(layout)
			require.NoError(err)
			catalog := newMaintenanceCatalog()
			noise := &maintenanceBenchNoiseReader{state: 1}
			for range 64 {
				written, err := loose.Write(b.Context(), io.LimitReader(noise, tt.size), WriteOptions{
					Durability: AtomicPublication, Dedup: VerifyFullHash, MaxBytes: tt.size,
				})
				require.NoError(err)
				catalog.addLoose(written.Hash, written.Path)
			}
			setup := newMaintainerForTest(b, catalog, layout, DefaultLimits())
			stats, err := setup.Pack(b.Context(), PackOptions{})
			require.NoError(err)
			require.Equal(64, stats.BlobsPacked)
			require.NoError(setup.Close())

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				written, err := loose.Write(b.Context(), io.LimitReader(noise, 4<<10), WriteOptions{
					Durability: AtomicPublication, Dedup: VerifyFullHash, MaxBytes: 4 << 10,
				})
				require.NoError(err)
				catalog.addLoose(written.Hash, written.Path)
				maintainer := newMaintainerForTest(b, catalog, layout, DefaultLimits())
				b.StartTimer()
				stats, err := maintainer.Pack(b.Context(), PackOptions{MaxBytes: 1 << 20})
				b.StopTimer()
				require.NoError(err)
				require.Equal(1, stats.BlobsPacked)
				require.NoError(maintainer.Close())
			}
		})
	}
}

type maintenanceBenchNoiseReader struct{ state uint32 }

func (r *maintenanceBenchNoiseReader) Read(p []byte) (int, error) {
	for i := range p {
		r.state ^= r.state << 13
		r.state ^= r.state >> 17
		r.state ^= r.state << 5
		p[i] = byte(r.state)
	}
	return len(p), nil
}
