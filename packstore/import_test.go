package packstore

import (
	"context"
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
