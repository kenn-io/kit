package packstore

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

// mixedFrameContents returns two compressible and two incompressible blobs, so
// merged sources hold both zstd and raw stored frames.
func mixedFrameContents() [][]byte {
	contents := make([][]byte, 0, 4)
	for i := range 2 {
		contents = append(contents, []byte(strings.Repeat(fmt.Sprintf("compressible line %d\n", i), 200)))
	}
	return append(contents, noiseContents(2)...)
}

func readFrameForTest(t *testing.T, layout Layout, entry IndexEntry) []byte {
	t.Helper()
	file, err := os.Open(layout.PackPath(entry.PackID))
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()
	frame := make([]byte, entry.StoredLen)
	_, err = file.ReadAt(frame, entry.Offset)
	require.NoError(t, err)
	return frame
}

func withMaxCopiedFrameBytes(t *testing.T, limit uint64) {
	t.Helper()
	previous := maxCopiedFrameBytes
	maxCopiedFrameBytes = limit
	t.Cleanup(func() { maxCopiedFrameBytes = previous })
}

func TestRepackMergeCopiesStoredFramesUnchanged(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	now := time.Now()
	contents := mixedFrameContents()
	sources := seedOneEntryPacks(t, layout, catalog, contents, now.Add(-48*time.Hour))
	var compressed int
	frames := make(map[Hash][]byte, len(sources))
	for _, source := range sources {
		if pack.BlobFlags(source.Flags)&pack.BlobCompressed != 0 {
			compressed++
		}
		frames[source.Hash] = readFrameForTest(t, layout, source)
	}
	require.Equal(t, 2, compressed, "the sources hold both compressed and raw frames")
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Repack(t.Context(), RepackOptions{
		TargetSize: 1 << 20, MaxBytes: 1 << 20, Now: now, Selection: RepackSelection{MergeBelowStored: 1 << 20},
	})
	require.NoError(t, err)
	assert.Equal(t, 4, stats.PacksMerged)
	assert.Equal(t, 1, stats.PacksSealed)
	entries, _ := catalog.snapshot()
	for i, source := range sources {
		merged := entries[source.Hash]
		assert.NotEqual(t, source.PackID, merged.PackID)
		assert.Equal(t, source.Flags, merged.Flags)
		assert.Equal(t, source.CRC32C, merged.CRC32C)
		assert.Equal(t, frames[source.Hash], readFrameForTest(t, layout, merged), "source %d frame is copied as stored", i)
		content := contents[slices.IndexFunc(contents, func(c []byte) bool { return hashForTest(c) == source.Hash })]
		got, _ := readStoreTest(t, maintainer.store, source.Hash)
		assert.Equal(t, content, got)
	}
}

func TestRepackMergeLeavesOutSourceWithCorruptFrame(t *testing.T) {
	for _, tc := range []struct {
		name      string
		copyLimit uint64
	}{
		{name: "copied frames", copyLimit: 16 << 20},
		{name: "recompressed frames", copyLimit: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withMaxCopiedFrameBytes(t, tc.copyLimit)
			layout := layoutForStoreTest(t)
			catalog := newMaintenanceCatalog()
			now := time.Now()
			contents := mixedFrameContents()
			sources := seedOneEntryPacks(t, layout, catalog, contents, now.Add(-48*time.Hour))
			damaged := sources[slices.IndexFunc(sources, func(e IndexEntry) bool {
				return pack.BlobFlags(e.Flags)&pack.BlobCompressed != 0
			})]
			// Flip one byte inside the compressed frame; the footer stays intact.
			file, err := os.OpenFile(layout.PackPath(damaged.PackID), os.O_RDWR, 0)
			require.NoError(t, err)
			at := damaged.Offset + damaged.StoredLen/2
			original := make([]byte, 1)
			_, err = file.ReadAt(original, at)
			require.NoError(t, err)
			_, err = file.WriteAt([]byte{original[0] ^ 0xff}, at)
			require.NoError(t, err)
			require.NoError(t, file.Close())
			maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

			stats, err := maintainer.Repack(t.Context(), RepackOptions{
				TargetSize: 1 << 20, MaxBytes: 1 << 20, Now: now, Selection: RepackSelection{MergeBelowStored: 1 << 20},
			})
			require.Error(t, err)
			require.True(t, isSourceContentError(err), "the run reports the damaged source: %v", err)
			require.ErrorContains(t, err, damaged.PackID)
			assert.Equal(t, 3, stats.PacksMerged)
			assert.Equal(t, 1, stats.PacksSealed)
			entries, packs := catalog.snapshot()
			assert.Equal(t, damaged, entries[damaged.Hash])
			assert.Contains(t, packs, damaged.PackID)
			for _, source := range sources {
				if source.Hash == damaged.Hash {
					continue
				}
				assert.NotEqual(t, source.PackID, entries[source.Hash].PackID)
				content := contents[slices.IndexFunc(contents, func(c []byte) bool { return hashForTest(c) == source.Hash })]
				got, _ := readStoreTest(t, maintainer.store, source.Hash)
				assert.Equal(t, content, got)
			}
		})
	}
}

// A pack whose frame matches its CRC but not its blob ID must not be copied:
// the CRC alone cannot prove the frame holds the content its ID names.
func TestRewriteSourcesRejectsFrameWithWrongContent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		content    []byte
		compressed bool
	}{
		{name: "compressed", content: []byte(strings.Repeat("claimed content\n", 200)), compressed: true},
		{name: "raw", content: noiseContents(1)[0], compressed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			layout := layoutForStoreTest(t)
			catalog := newMaintenanceCatalog()
			actual := append([]byte("different "), tc.content...)
			frame, compressed := pack.EncodeFrame(actual, pack.DefaultZstdLevel)
			require.Equal(tc.compressed, compressed)
			writer, err := pack.NewWriter(t.TempDir(), pack.WriterOptions{})
			require.NoError(err)
			_, err = writer.AppendEncoded(pack.ComputeBlobID(tc.content), frame, uint64(len(actual)), compressed)
			require.NoError(err)
			packID := writer.ID()
			sealed, err := writer.Seal(layout.PackPath(packID))
			require.NoError(err)
			entry := indexFromPack(packID, sealed[0])
			catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
			catalog.entries[entry.Hash] = entry
			maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

			result, err := maintainer.rewriteSources(t.Context(), []IndexEntry{entry}, 0)
			require.ErrorIs(err, pack.ErrBlobMismatch)
			failed, ok := errors.AsType[*sourceReadError](err)
			require.True(ok)
			assert.Equal(t, packID, failed.packID)
			assert.Empty(t, result.records)
			assert.Empty(t, result.moves)
		})
	}
}
