package packstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

type recordingRestoreCatalog struct {
	calls     int
	records   []PackRecord
	adoptions []Adoption
	err       error
}

func (c *recordingRestoreCatalog) ReplaceRestoredPacks(
	_ context.Context,
	records []PackRecord,
	adoptions []Adoption,
) error {
	c.calls++
	c.records = append([]PackRecord(nil), records...)
	c.adoptions = append([]Adoption(nil), adoptions...)
	return c.err
}

func TestPrepareImportPublishesBeforeCatalogAuthority(t *testing.T) {
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))

	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

	require.NoError(t, err)
	assert.Equal(t, []Hash{hashFromEntry(t, entries[0])}, prepared.PackedHashes())
	_, err = target.Stat(importPackPath("content", packID))
	assert.NoError(t, err)
	catalog := &recordingRestoreCatalog{}
	assert.Equal(t, 0, catalog.calls)
	assertNoImportStaging(t, target)
}

func TestPrepareImportReusesByteIdenticalDestination(t *testing.T) {
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
	opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
	first, err := PrepareImport(context.Background(), target, "content", input, opts)
	require.NoError(t, err)

	second, err := PrepareImport(context.Background(), target, "content", input, opts)

	require.NoError(t, err)
	assert.Equal(t, first.PackedHashes(), second.PackedHashes())
	assert.Equal(t, first.Stats(), second.Stats())
}

func TestPrepareImportPublishesWhenHardLinksUnavailable(t *testing.T) {
	originalLink := importRootLink
	importRootLink = func(*os.Root, string, string) error { return errors.New("hard links unavailable") }
	t.Cleanup(func() { importRootLink = originalLink })
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))

	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

	require.NoError(t, err)
	assert.Equal(t, []Hash{hashFromEntry(t, entries[0])}, prepared.PackedHashes())
	_, err = target.Stat(importPackPath("content", packID))
	assert.NoError(t, err)
	assertNoImportStaging(t, target)
}

func TestPrepareImportLinkFallbackRefusesPreexistingDestination(t *testing.T) {
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
	opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
	_, err := PrepareImport(context.Background(), target, "content", input, opts)
	require.NoError(t, err)
	final := filepath.Join(target.Name(), filepath.FromSlash(importPackPath("content", packID)))
	require.NoError(t, os.WriteFile(final, []byte("preexisting collision"), 0o600))
	originalLink := importRootLink
	importRootLink = func(*os.Root, string, string) error { return errors.New("hard links unavailable") }
	t.Cleanup(func() { importRootLink = originalLink })

	prepared, err := PrepareImport(context.Background(), target, "content", input, opts)

	assert.Nil(t, prepared)
	assert.ErrorContains(t, err, "collision")
	data, readErr := os.ReadFile(final)
	require.NoError(t, readErr)
	assert.Equal(t, []byte("preexisting collision"), data)
}

func TestPrepareImportRefusesPackIDCollision(t *testing.T) {
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
	opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
	_, err := PrepareImport(context.Background(), target, "content", input, opts)
	require.NoError(t, err)
	final := filepath.Join(target.Name(), filepath.FromSlash(importPackPath("content", packID)))
	require.NoError(t, os.WriteFile(final, []byte("different bytes"), 0o600))

	prepared, err := PrepareImport(context.Background(), target, "content", input, opts)

	assert.Nil(t, prepared)
	assert.ErrorContains(t, err, "collision")
	data, readErr := os.ReadFile(final)
	require.NoError(t, readErr)
	assert.Equal(t, []byte("different bytes"), data)
}

func TestPrepareImportRejectsOverlappingFullFooterEntries(t *testing.T) {
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"), []byte("unselected"))
	data, err := os.ReadFile(source)
	require.NoError(t, err)
	trailerStart := len(data) - plainPackTrailerSize
	footerLen := int(binary.LittleEndian.Uint32(data[trailerStart:]))
	footerStart := trailerStart - footerLen
	secondOffset := footerStart + 4 + plainPackEntrySize + 32
	binary.LittleEndian.PutUint64(data[secondOffset:], entries[0].Offset)
	digest := sha256.Sum256(data[footerStart : trailerStart+4])
	copy(data[trailerStart+4:trailerStart+36], digest[:])
	require.NoError(t, os.WriteFile(source, data, 0o600))

	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries[:1]),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

	assert.Nil(t, prepared)
	assert.ErrorIs(t, err, pack.ErrCorrupt)
	_, statErr := target.Stat(importPackPath("content", packID))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestPreparedImportRejectsOverflowingFullFooterTotals(t *testing.T) {
	_, _, err := importCatalogPlan(preparedImportPack{
		pack:    ImportPack{PackID: pack.NewPackID()},
		entries: []pack.Entry{{Offset: pack.MinEntryOffset, StoredLen: ^uint64(0)}},
	}, time.Now())

	assert.ErrorIs(t, err, pack.ErrCorrupt)
}

func TestPreparedImportCatalogFailureLeavesPublishedOrphan(t *testing.T) {
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})
	require.NoError(t, err)
	catalogErr := errors.New("transaction failed")
	catalog := &recordingRestoreCatalog{err: catalogErr}

	err = prepared.Commit(context.Background(), catalog)

	assert.ErrorIs(t, err, catalogErr)
	assert.ErrorContains(t, err, "catalog")
	assert.Equal(t, 1, catalog.calls)
	_, statErr := target.Stat(importPackPath("content", packID))
	assert.NoError(t, statErr)
}

func TestPreparedImportRecordsFullFooterTotalsForSelectedSubset(t *testing.T) {
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"), []byte("unselected sibling"))
	createdAt := time.Now().UTC().Truncate(time.Second)
	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries[:1]),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: createdAt})
	require.NoError(t, err)
	catalog := &recordingRestoreCatalog{}

	require.NoError(t, prepared.Commit(context.Background(), catalog))

	require.Len(t, catalog.records, 1)
	assert.Equal(t, PackRecord{
		PackID: packID, EntryCount: 2,
		StoredBytes: int64(entries[0].StoredLen + entries[1].StoredLen), //nolint:gosec // test pack is small
		CreatedAt:   createdAt,
	}, catalog.records[0])
	require.Len(t, catalog.adoptions, 1)
	assert.Equal(t, hashFromEntry(t, entries[0]), catalog.adoptions[0].Entry.Hash)
	assert.Equal(t, entries[0].CRC32C, catalog.adoptions[0].Entry.CRC32C)
	assert.Equal(t, []string{entries[0].ID.String()}, catalog.adoptions[0].OriginalHashes)
	assert.Equal(t, 1, catalog.calls)
}

func TestPreparedImportCommitValidatesInputsAndAllowsIdempotentRetry(t *testing.T) {
	var nilPrepared *PreparedImport
	assert.ErrorContains(t, nilPrepared.Commit(context.Background(), &recordingRestoreCatalog{}), "nil")

	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})
	require.NoError(t, err)
	assert.ErrorContains(t, prepared.Commit(context.Background(), nil), "nil")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, prepared.Commit(canceled, &recordingRestoreCatalog{}), context.Canceled)
	catalog := &recordingRestoreCatalog{}
	require.NoError(t, prepared.Commit(context.Background(), catalog))
	require.NoError(t, prepared.Commit(context.Background(), catalog))
	assert.Equal(t, 2, catalog.calls)
}

func TestPreparedImportRetryAcrossMaintainerOrphanDisposition(t *testing.T) {
	t.Run("adopted orphan is reusable", func(t *testing.T) {
		target := openImportTarget(t)
		source, packID, entries := buildImportTestPack(t, []byte("selected"))
		input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
		opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
		_, err := PrepareImport(context.Background(), target, "content", input, opts)
		require.NoError(t, err)
		catalog := newMaintenanceCatalog()
		hash := hashFromEntry(t, entries[0])
		catalog.members[hash] = Reference{Hash: hash, OriginalHashes: []string{hash.String()}}

		stats := runImportMaintainer(t, target, catalog, DefaultLimits())

		assert.Equal(t, 1, stats.PacksAdopted)
		retried, err := PrepareImport(context.Background(), target, "content", input, opts)
		require.NoError(t, err)
		assert.Equal(t, []Hash{hash}, retried.PackedHashes())
	})

	t.Run("removed orphan is recopied", func(t *testing.T) {
		target := openImportTarget(t)
		source, packID, entries := buildImportTestPack(t, []byte("selected"))
		input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
		opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
		_, err := PrepareImport(context.Background(), target, "content", input, opts)
		require.NoError(t, err)

		stats := runImportMaintainer(t, target, newMaintenanceCatalog(), DefaultLimits())

		assert.Equal(t, 1, stats.PacksRemoved)
		_, err = target.Stat(importPackPath("content", packID))
		assert.ErrorIs(t, err, os.ErrNotExist)
		retried, err := PrepareImport(context.Background(), target, "content", input, opts)
		require.NoError(t, err)
		assert.Equal(t, []Hash{hashFromEntry(t, entries[0])}, retried.PackedHashes())
		_, err = target.Stat(importPackPath("content", packID))
		assert.NoError(t, err)
	})

	t.Run("oversized retained orphan is reusable with compatible target limits", func(t *testing.T) {
		target := openImportTarget(t)
		source, packID, entries := buildImportTestPack(t, []byte("selected content larger than maintenance ceiling"))
		input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
		opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
		_, err := PrepareImport(context.Background(), target, "content", input, opts)
		require.NoError(t, err)
		catalog := newMaintenanceCatalog()
		hash := hashFromEntry(t, entries[0])
		catalog.members[hash] = Reference{Hash: hash, OriginalHashes: []string{hash.String()}}
		maintenanceLimits := DefaultLimits()
		maintenanceLimits.BlobBytes = 8

		stats := runImportMaintainer(t, target, catalog, maintenanceLimits)

		assert.Equal(t, 1, stats.PacksDeferredOversized)
		retried, err := PrepareImport(context.Background(), target, "content", input, opts)
		require.NoError(t, err)
		assert.Equal(t, []Hash{hash}, retried.PackedHashes())
	})

	t.Run("damaged retained orphan fails current selection verification", func(t *testing.T) {
		target := openImportTarget(t)
		source, packID, entries := buildImportTestPack(t, []byte("selected"))
		input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
		opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
		_, err := PrepareImport(context.Background(), target, "content", input, opts)
		require.NoError(t, err)
		final, err := target.OpenFile(importPackPath("content", packID), os.O_RDWR, 0)
		require.NoError(t, err)
		_, err = final.WriteAt([]byte{0xff}, int64(entries[0].Offset)) //nolint:gosec // test pack is small
		require.NoError(t, err)
		require.NoError(t, final.Close())
		catalog := newMaintenanceCatalog()
		hash := hashFromEntry(t, entries[0])
		catalog.members[hash] = Reference{Hash: hash, OriginalHashes: []string{hash.String()}}

		stats := runImportMaintainer(t, target, catalog, DefaultLimits())

		assert.Equal(t, 1, stats.PacksQuarantined)
		_, err = target.Stat(importPackPath("content", packID))
		require.NoError(t, err)
		prepared, err := PrepareImport(context.Background(), target, "content", input, opts)
		assert.Nil(t, prepared)
		assert.ErrorContains(t, err, "collision")
	})
}

func runImportMaintainer(t *testing.T, target *os.Root, catalog *maintenanceCatalog, limits Limits) PackStats {
	t.Helper()
	layout, err := NewLayout(filepath.Join(target.Name(), "content"), LayoutOptions{
		Staging: StagingStoreDirectory, StagingDir: ".staging",
	})
	require.NoError(t, err)
	maintainer, err := NewMaintainer(catalog, layout, MaintainerOptions{Limits: limits})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, maintainer.Close()) })
	stats, err := maintainer.Pack(context.Background(), PackOptions{})
	require.NoError(t, err)
	return stats
}

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

func TestPrepareImportFallsBackWholePackForRecognizableUnsupportedEncoding(t *testing.T) {
	for _, test := range []struct {
		name   string
		offset int64
		value  byte
	}{
		{name: "version", offset: 4, value: plainPackVersion + 1},
		{name: "flags", offset: 5, value: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := openImportTarget(t)
			path, packID, entries := buildImportTestPack(t, []byte("first"), []byte("second"))
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			require.NoError(t, err)
			_, err = f.WriteAt([]byte{test.value}, test.offset)
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

func assertNoImportStaging(t *testing.T, target *os.Root) {
	t.Helper()
	packEntries, err := os.ReadDir(filepath.Join(target.Name(), "content", "packs"))
	require.NoError(t, err)
	for _, entry := range packEntries {
		assert.False(t, strings.HasSuffix(entry.Name(), ".staging"), entry.Name())
	}
}
