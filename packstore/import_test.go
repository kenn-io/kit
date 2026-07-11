package packstore

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
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

func TestPrepareImportFallsBackWholePackForUnsupportedEncoding(t *testing.T) {
	target := openImportTarget(t)
	path, packID, entries := buildImportTestPack(t, []byte("first"), []byte("second"))
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{1}, 5)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

	require.NoError(t, err)
	assert.Empty(t, prepared.PackedHashes())
	assert.Equal(t, ImportStats{
		Fallbacks: []ImportFallback{{PackID: packID, Reason: FallbackPackEncoding}},
	}, prepared.Stats())
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
