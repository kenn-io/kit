package packstore

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkLooseWriteRaw(b *testing.B) {
	benchmarkLooseWrite(b, false)
}

func BenchmarkLooseWriteCompressed(b *testing.B) {
	benchmarkLooseWrite(b, true)
}

func benchmarkLooseWrite(b *testing.B, compressed bool) {
	content := representativeNDJSON(4_000)
	layout, err := NewLayout(b.TempDir(), LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(b, err)
	loose, err := NewLooseStore(layout)
	require.NoError(b, err)
	opts := WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
		Compression: LooseCompressionOptions{
			Enabled: compressed,
		},
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(content)))
	b.ResetTimer()
	for range b.N {
		result, writeErr := loose.WriteBytes(context.Background(), content, opts)
		require.NoError(b, writeErr)
		b.StopTimer()
		require.NoError(b, loose.Remove(result.Hash, BestEffortRemoval))
		b.StartTimer()
	}
}

func BenchmarkCompressedLooseStreamingRead(b *testing.B) {
	content := representativeNDJSON(8_000)
	layout, err := NewLayout(b.TempDir(), LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(b, err)
	loose, err := NewLooseStore(layout)
	require.NoError(b, err)
	result, err := loose.WriteBytes(context.Background(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
		Compression: LooseCompressionOptions{
			Enabled: true,
		},
	})
	require.NoError(b, err)
	resolver := &mapResolver{locations: map[Hash]Location{result.Hash: {Member: true}}}
	store, err := NewStore(resolver, layout, StoreOptions{})
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	buffer := make([]byte, 64<<10)
	b.ReportAllocs()
	b.SetBytes(int64(len(content)))
	b.ResetTimer()
	for range b.N {
		stream, _, openErr := store.OpenStream(context.Background(), result.Hash)
		require.NoError(b, openErr)
		_, copyErr := io.CopyBuffer(io.Discard, stream, buffer)
		require.NoError(b, copyErr)
		require.NoError(b, stream.Close())
	}
}

func BenchmarkCompressedLoosePackIngestion(b *testing.B) {
	content := bytes.Repeat([]byte("{\"type\":\"message\",\"content\":\"pack ingestion benchmark\"}\n"), 8_000)
	base := b.TempDir()
	b.ReportAllocs()
	b.SetBytes(int64(len(content)))
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		root, err := os.MkdirTemp(base, "iteration-")
		require.NoError(b, err)
		layout, err := NewLayout(root, LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
		require.NoError(b, err)
		loose, err := NewLooseStore(layout)
		require.NoError(b, err)
		written, err := loose.WriteBytes(context.Background(), content, WriteOptions{
			Durability: AtomicPublication,
			Dedup:      VerifyFullHash,
			Compression: LooseCompressionOptions{
				Enabled: true,
			},
		})
		require.NoError(b, err)
		catalog := newMaintenanceCatalog()
		catalog.members[written.Hash] = Reference{
			Hash: written.Hash, OriginalHashes: []string{written.Hash.String()},
		}
		catalog.candidates[written.Hash] = Candidate{
			Hash: written.Hash, OriginalHashes: []string{written.Hash.String()},
			Paths: []string{filepath.Clean(written.Path)}, Size: written.Size,
		}
		maintainer, err := NewMaintainer(catalog, layout, MaintainerOptions{})
		require.NoError(b, err)
		b.StartTimer()

		stats, packErr := maintainer.Pack(context.Background(), PackOptions{})
		require.NoError(b, packErr)
		require.Equal(b, 1, stats.BlobsPacked)

		b.StopTimer()
		require.NoError(b, maintainer.Close())
		require.NoError(b, os.RemoveAll(root))
		b.StartTimer()
	}
}
