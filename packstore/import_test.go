package packstore

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestPrepareImportUsesConfiguredLimits(t *testing.T) {
	target := openImportTarget(t)
	contents := [][]byte{[]byte("small"), []byte("larger selected content")}
	path, packID, entries := buildImportTestPack(t, contents...)
	limits := DefaultLimits()
	limits.BlobBytes = int64(len(contents[1]))

	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	require.NoError(t, err)
	assert.ElementsMatch(t, []Hash{hashFromEntry(t, entries[0]), hashFromEntry(t, entries[1])}, prepared.PackedHashes())
	assert.Equal(t, ImportStats{PackedPacks: 1, PackedBlobs: 2}, prepared.Stats())
}

func TestPrepareImportFallsBackWholePackForContainerLimit(t *testing.T) {
	target := openImportTarget(t)
	path, packID, entries := buildImportTestPack(t, []byte("first"), []byte("second"))
	info, err := os.Stat(path)
	require.NoError(t, err)
	limits := DefaultLimits()
	limits.PackBytes = info.Size() - 1

	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	require.NoError(t, err)
	assert.Empty(t, prepared.PackedHashes())
	assert.Equal(t, ImportStats{
		Fallbacks: []ImportFallback{{PackID: packID, Reason: FallbackPackContainerLimit}},
	}, prepared.Stats())
}

func TestPrepareImportRejectsOversizedNonPackInsteadOfFallingBack(t *testing.T) {
	target := openImportTarget(t)
	path := filepath.Join(t.TempDir(), "not-a-pack")
	require.NoError(t, os.WriteFile(path, make([]byte, 1024), 0o600))
	_, packID, entries := buildImportTestPack(t, []byte("selected"))
	limits := DefaultLimits()
	limits.PackBytes = 512

	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	assert.Nil(t, prepared)
	assert.ErrorIs(t, err, pack.ErrBadMagic)
}

func TestPrepareImportRejectsForgedOversizedFooterLength(t *testing.T) {
	target := openImportTarget(t)
	path, packID, entries := buildImportTestPack(t, []byte("selected"))
	info, err := os.Stat(path)
	require.NoError(t, err)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	var forged [4]byte
	binary.LittleEndian.PutUint32(forged[:], uint32(info.Size()+1))
	_, err = f.WriteAt(forged[:], info.Size()-plainPackTrailerSize)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	limits := DefaultLimits()
	limits.FooterBytes = 1

	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	assert.Nil(t, prepared)
	assert.ErrorIs(t, err, pack.ErrTruncated)
}

func TestPrepareImportRejectsForgedOversizedFooterCount(t *testing.T) {
	target := openImportTarget(t)
	path, packID, entries := buildImportTestPack(t, []byte("selected"))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	trailerStart := len(data) - plainPackTrailerSize
	footerLen := int(binary.LittleEndian.Uint32(data[trailerStart:]))
	footerStart := trailerStart - footerLen
	binary.LittleEndian.PutUint32(data[footerStart:], 2)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	limits := DefaultLimits()
	limits.PackEntries = 1

	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	assert.Nil(t, prepared)
	assert.ErrorIs(t, err, pack.ErrCorrupt)
}

func TestPrepareImportRejectsMetadataMismatchBehindContainerLimit(t *testing.T) {
	target := openImportTarget(t)
	path, packID, entries := buildImportTestPack(t, []byte("selected"))
	info, err := os.Stat(path)
	require.NoError(t, err)
	selections := importSelections(t, entries)
	selections[0].Offset++
	limits := DefaultLimits()
	limits.PackBytes = info.Size() - 1

	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: selections,
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	assert.Nil(t, prepared)
	assert.ErrorIs(t, err, pack.ErrCorrupt)
}

func TestPrepareImportFallsBackWholePackForValidFooterLimits(t *testing.T) {
	for _, dimension := range []string{"footer bytes", "entry count"} {
		t.Run(dimension, func(t *testing.T) {
			target := openImportTarget(t)
			path, packID, entries := buildImportTestPack(t, []byte("first"), []byte("second"))
			limits := DefaultLimits()
			var reason FallbackReason
			switch dimension {
			case "footer bytes":
				info, err := os.Stat(path)
				require.NoError(t, err)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				footerLen := int64(binary.LittleEndian.Uint32(data[info.Size()-plainPackTrailerSize:]))
				limits.FooterBytes = footerLen - 1
				reason = FallbackPackFooterLimit
			case "entry count":
				limits.PackEntries = 1
				reason = FallbackPackEntryCountLimit
			}

			prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
				PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
			}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

			require.NoError(t, err)
			assert.Empty(t, prepared.PackedHashes())
			assert.Equal(t, ImportStats{
				Fallbacks: []ImportFallback{{PackID: packID, Reason: reason}},
			}, prepared.Stats())
		})
	}
}

func TestPrepareImportFallsBackOnlyOversizedSelectedEntry(t *testing.T) {
	target := openImportTarget(t)
	contents := [][]byte{[]byte("small"), []byte("larger selected content")}
	path, packID, entries := buildImportTestPack(t, contents...)
	limits := DefaultLimits()
	limits.BlobBytes = int64(len(contents[0]))

	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	require.NoError(t, err)
	assert.Equal(t, []Hash{hashFromEntry(t, entries[0])}, prepared.PackedHashes())
	assert.Equal(t, ImportStats{
		PackedPacks: 1,
		PackedBlobs: 1,
		Fallbacks: []ImportFallback{{
			PackID: packID,
			Hash:   hashFromEntry(t, entries[1]),
			Reason: FallbackBlobLimit,
		}},
	}, prepared.Stats())
}

func TestPrepareImportLimitFallbackStillVerifiesSelectedPayload(t *testing.T) {
	for _, dimension := range []string{"container bytes", "footer bytes", "entry count"} {
		t.Run(dimension, func(t *testing.T) {
			target := openImportTarget(t)
			path, packID, entries := buildImportTestPack(t, []byte("selected content"), []byte("sibling"))
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			require.NoError(t, err)
			_, err = f.WriteAt([]byte{0xff}, int64(entries[0].Offset))
			require.NoError(t, err)
			require.NoError(t, f.Close())
			limits := DefaultLimits()
			switch dimension {
			case "container bytes":
				info, err := os.Stat(path)
				require.NoError(t, err)
				limits.PackBytes = info.Size() - 1
			case "footer bytes":
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				trailerStart := len(data) - plainPackTrailerSize
				limits.FooterBytes = int64(binary.LittleEndian.Uint32(data[trailerStart:])) - 1
			case "entry count":
				limits.PackEntries = 1
			}

			prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
				PackID: packID, SourcePath: path, Selections: importSelections(t, entries[:1]),
			}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

			assert.Nil(t, prepared)
			assert.ErrorIs(t, err, pack.ErrCorrupt)
		})
	}
}

func TestPrepareImportLimitFallbackSkipsOversizedSelectedPayload(t *testing.T) {
	target := openImportTarget(t)
	content := []byte("oversized selected content")
	path, packID, entries := buildImportTestPack(t, content)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{0xff}, int64(entries[0].Offset))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	info, err := os.Stat(path)
	require.NoError(t, err)
	limits := DefaultLimits()
	limits.PackBytes = info.Size() - 1
	limits.BlobBytes = int64(len(content) - 1)

	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	require.NoError(t, err)
	assert.Empty(t, prepared.PackedHashes())
	assert.Equal(t, ImportStats{
		Fallbacks: []ImportFallback{{PackID: packID, Reason: FallbackPackContainerLimit}},
	}, prepared.Stats())
}

func TestPrepareImportStreamingVerifierCleansScratch(t *testing.T) {
	originalChunk := importVerifyIDChunkEntries
	importVerifyIDChunkEntries = 1
	t.Cleanup(func() { importVerifyIDChunkEntries = originalChunk })

	t.Run("success", func(t *testing.T) {
		target := openImportTarget(t)
		path, packID, entries := buildImportTestPack(t, []byte("first"), []byte("second"), []byte("third"))
		limits := DefaultLimits()
		limits.PackEntries = 1

		_, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
			PackID: packID, SourcePath: path, Selections: importSelections(t, entries[:1]),
		}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

		require.NoError(t, err)
		assertNoImportVerificationScratch(t, target)
	})

	t.Run("cross-run duplicate", func(t *testing.T) {
		target := openImportTarget(t)
		dir := t.TempDir()
		writer, err := pack.NewWriter(dir, pack.WriterOptions{})
		require.NoError(t, err)
		entry, err := writer.Append([]byte("duplicate"))
		require.NoError(t, err)
		_, err = writer.Append([]byte("middle"))
		require.NoError(t, err)
		_, err = writer.Append([]byte("duplicate"))
		require.NoError(t, err)
		packPath := filepath.Join(dir, writer.ID()+PackExt)
		_, err = writer.Seal(packPath)
		require.NoError(t, err)
		limits := DefaultLimits()
		limits.PackEntries = 1

		prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
			PackID: writer.ID(), SourcePath: packPath, Selections: importSelections(t, []pack.Entry{entry}),
		}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

		assert.Nil(t, prepared)
		require.ErrorIs(t, err, pack.ErrCorrupt)
		assertNoImportVerificationScratch(t, target)
	})
}

func TestPrepareImportRejectsCorruptSourceInsteadOfFallingBack(t *testing.T) {
	target := openImportTarget(t)
	path, packID, entries := buildImportTestPack(t, []byte("selected content"), []byte("unselected content"))
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{0xff}, int64(entries[0].Offset))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries[:1]),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

	assert.Nil(t, prepared)
	assert.ErrorIs(t, err, pack.ErrCorrupt)
}

func buildImportTestPack(t *testing.T, contents ...[]byte) (string, string, []pack.Entry) {
	t.Helper()
	dir := t.TempDir()
	writer, err := pack.NewWriter(dir, pack.WriterOptions{})
	require.NoError(t, err)
	for _, content := range contents {
		_, err = writer.Append(content)
		require.NoError(t, err)
	}
	path := filepath.Join(dir, writer.ID()+PackExt)
	entries, err := writer.Seal(path)
	require.NoError(t, err)
	return path, writer.ID(), entries
}

func openImportTarget(t *testing.T) *os.Root {
	t.Helper()
	target, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	return target
}

func importSelections(t *testing.T, entries []pack.Entry) []ImportSelection {
	t.Helper()
	selections := make([]ImportSelection, len(entries))
	for i, entry := range entries {
		selections[i] = ImportSelection{
			Hash:      hashFromEntry(t, entry),
			RawLen:    int64(entry.RawLen), //nolint:gosec // test packs are small
			Offset:    entry.Offset,
			StoredLen: entry.StoredLen,
			Flags:     uint8(entry.Flags),
		}
	}
	return selections
}

func hashFromEntry(t *testing.T, entry pack.Entry) Hash {
	t.Helper()
	hash, err := ParseHash(entry.ID.String())
	require.NoError(t, err)
	return hash
}

func assertNoImportVerificationScratch(t *testing.T, target *os.Root) {
	t.Helper()
	entries, err := os.ReadDir(target.Name())
	require.NoError(t, err)
	for _, entry := range entries {
		assert.False(t, strings.HasPrefix(entry.Name(), importVerifyScratchPrefix), entry.Name())
	}
}
