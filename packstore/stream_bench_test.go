package packstore

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
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

func benchmarkStorePackedReads(b *testing.B, content []byte) {
	b.Helper()
	root := b.TempDir()
	layout, err := NewLayout(root, LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(b, err)
	w, err := pack.NewWriter(b.TempDir(), pack.WriterOptions{})
	require.NoError(b, err)
	entry, err := w.Append(content)
	require.NoError(b, err)
	packID := w.ID()
	require.NoError(b, os.MkdirAll(filepath.Dir(layout.PackPath(packID)), 0o700))
	_, err = w.Seal(layout.PackPath(packID))
	require.NoError(b, err)
	hash, err := ParseHash(entry.ID.String())
	require.NoError(b, err)
	indexed := IndexEntry{Hash: hash, PackID: packID, Offset: int64(entry.Offset), StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen), Flags: uint8(entry.Flags), CRC32C: entry.CRC32C}
	store, err := NewStore(&mapResolver{locations: map[Hash]Location{
		hash: {Member: true, Pack: &indexed},
	}}, layout, StoreOptions{})
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })

	for _, mode := range []string{"stream", "buffered"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(content)))
			for range b.N {
				if mode == "stream" {
					reader, _, openErr := store.OpenStream(context.Background(), hash)
					require.NoError(b, openErr)
					_, copyErr := io.Copy(io.Discard, reader)
					require.NoError(b, copyErr)
					continue
				}
				reader, _, openErr := store.Open(context.Background(), hash)
				require.NoError(b, openErr)
				_, copyErr := io.Copy(io.Discard, reader)
				require.NoError(b, copyErr)
				require.NoError(b, reader.Close())
			}
		})
	}
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
			layout, err := NewLayout(b.TempDir(), LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
			require.NoError(b, err)
			loose, err := NewLooseStore(layout)
			require.NoError(b, err)
			written, err := loose.Write(context.Background(), tt.source(), WriteOptions{
				Durability: AtomicPublication, Dedup: VerifyFullHash, MaxBytes: size,
			})
			require.NoError(b, err)
			catalog := newMaintenanceCatalog()
			catalog.addLoose(written.Hash, written.Path)
			maintainer := newMaintainerForTest(b, catalog, layout, DefaultLimits())

			b.ReportAllocs()
			b.SetBytes(2 * size)
			b.ResetTimer()
			for range b.N {
				_, err := maintainer.Pack(context.Background(), PackOptions{})
				require.NoError(b, err)
				_, err = maintainer.Unpack(context.Background())
				require.NoError(b, err)
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
