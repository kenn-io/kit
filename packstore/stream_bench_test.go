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
