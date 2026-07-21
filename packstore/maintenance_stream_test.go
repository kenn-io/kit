package packstore

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestPackAndUnpackStreamAboveFormerCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("streams an object above the former 64 MiB maintenance ceiling")
	}
	size := largeStoreStreamTestBytes(t, 64<<20+1)
	layout := layoutForStoreTest(t)
	loose, err := NewLooseStore(layout)
	require.NoError(t, err)
	written, err := loose.Write(context.Background(), io.LimitReader(streamZeroReader{}, size), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash, MaxBytes: size,
	})
	require.NoError(t, err)
	require.Equal(t, size, written.Size)

	catalog := newMaintenanceCatalog()
	catalog.addLoose(written.Hash, written.Path)
	limits := DefaultLimits()
	limits.BlobBytes = size
	maintainer := newMaintainerForTest(t, catalog, layout, limits)

	packed, err := maintainer.Pack(context.Background(), PackOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, packed.BlobsPacked)
	assert.Equal(t, size, packed.BytesPacked)
	assert.NoFileExists(t, written.Path)

	unpacked, err := maintainer.Unpack(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, unpacked.BlobsRestored)
	assert.Equal(t, size, unpacked.BytesRestored)
	info, err := os.Stat(written.Path)
	require.NoError(t, err)
	assert.Equal(t, size, info.Size())
	err = verifyLoosePath(context.Background(), written.Path, written.Hash, size)
	require.NoError(t, err)
}

func TestPackCompressedLooseStreamAboveFormerCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("streams a compressed object above the former 64 MiB maintenance ceiling")
	}
	size := largeStoreStreamTestBytes(t, 64<<20+1)
	layout := layoutForStoreTest(t)
	loose, err := NewLooseStore(layout)
	require.NoError(t, err)
	written, err := loose.Write(context.Background(), io.LimitReader(streamZeroReader{}, size), WriteOptions{
		Durability:  AtomicPublication,
		Dedup:       VerifyFullHash,
		MaxBytes:    size,
		Compression: LooseCompressionOptions{Enabled: true},
	})
	require.NoError(t, err)
	require.Equal(t, LooseEncodingZstd, written.Encoding)
	require.Less(t, written.StoredSize, written.Size)

	catalog := newMaintenanceCatalog()
	addMaintenanceCandidate(catalog, Candidate{
		Hash: written.Hash, Paths: []string{written.Path}, Size: written.Size,
	})
	limits := DefaultLimits()
	limits.BlobBytes = size
	maintainer := newMaintainerForTest(t, catalog, layout, limits)

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1, stats.BlobsPacked)
	assert.Equal(t, size, stats.BytesPacked)
	assert.NoFileExists(t, written.Path)
	stream, gotSize, err := maintainer.store.OpenStream(context.Background(), written.Hash)
	require.NoError(t, err)
	assert.Equal(t, size, gotSize)
	require.NoError(t, stream.Verify())
	require.NoError(t, stream.Close())
}

func TestRepackStreamsAboveFormerCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("streams an object above the former 64 MiB maintenance ceiling")
	}
	size := largeStoreStreamTestBytes(t, 64<<20+1)
	layout := layoutForStoreTest(t)
	require.NoError(t, os.MkdirAll(layout.PacksDir(), 0o700))
	writer, err := pack.NewWriter(layout.PacksDir(), pack.WriterOptions{})
	require.NoError(t, err)
	live, err := writer.AppendStream(context.Background(), io.LimitReader(streamZeroReader{}, size), uint64(size), pack.AppendStreamOptions{
		ScratchDir: layout.PacksDir(),
	})
	require.NoError(t, err)
	_, err = writer.Append([]byte("dead-one"))
	require.NoError(t, err)
	_, err = writer.Append([]byte("dead-two"))
	require.NoError(t, err)
	packID := writer.ID()
	entries, err := writer.Seal(layout.PackPath(packID))
	require.NoError(t, err)

	indexed := indexFromPack(packID, live)
	catalog := newMaintenanceCatalog()
	catalog.members[indexed.Hash] = Reference{Hash: indexed.Hash}
	catalog.entries[indexed.Hash] = indexed
	record := PackRecord{PackID: packID, EntryCount: int64(len(entries)), CreatedAt: time.Now().Add(-time.Hour)}
	for _, entry := range entries {
		record.StoredBytes += int64(entry.StoredLen) //nolint:gosec // test entries are bounded
	}
	catalog.packs[packID] = record
	limits := DefaultLimits()
	limits.BlobBytes = size
	maintainer := newMaintainerForTest(t, catalog, layout, limits)

	stats, err := maintainer.Repack(context.Background(), RepackOptions{
		Now: time.Now(), Selection: RepackSelection{MinAge: time.Nanosecond, MinDeadStored: 1},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, stats.BlobsRepacked)
	assert.Equal(t, size, stats.BytesRepacked)
	assert.NoFileExists(t, layout.PackPath(packID))
	stream, gotSize, err := maintainer.store.OpenStream(context.Background(), indexed.Hash)
	require.NoError(t, err)
	assert.Equal(t, size, gotSize)
	require.NoError(t, stream.Verify())
	require.NoError(t, stream.Close())
}
