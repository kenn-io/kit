package packstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestPackRepairsThenPacksAndSweepsLooseContent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := []byte("pack this loose content")
	hash := writeMaintenanceLoose(t, layout, content)
	catalog.addLoose(hash, layout.LoosePath(hash))
	require.NoError(os.MkdirAll(layout.PacksDir(), 0o700))
	staleStaging := filepath.Join(layout.PacksDir(), "interrupted.staging")
	require.NoError(os.WriteFile(staleStaging, []byte("partial"), 0o600))

	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	stats, err := maintainer.Pack(context.Background(), PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.PacksSealed)
	assert.Equal(1, stats.BlobsPacked)
	assert.Equal(int64(len(content)), stats.BytesPacked)
	assert.Zero(stats.LooseSwept, "normal source cleanup is part of packing, not the redundant-loose sweep")
	assert.NoFileExists(layout.LoosePath(hash))
	assert.NoFileExists(staleStaging)

	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got)
	require.NoError(os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
	require.NoError(os.WriteFile(layout.LoosePath(hash), content, 0o600))
	stats, err = maintainer.Pack(context.Background(), PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.LooseSwept, "a later redundant loose copy is sweep work")
	assert.NoFileExists(layout.LoosePath(hash))
}

func TestPackDefersOversizedBlobWithoutFailingRun(t *testing.T) {
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := bytes.Repeat([]byte("x"), 9)
	hash := writeMaintenanceLoose(t, layout, content)
	catalog.addLoose(hash, layout.LoosePath(hash))
	limits := DefaultLimits()
	limits.BlobBytes = 8
	maintainer := newMaintainerForTest(t, catalog, layout, limits)

	stats, err := maintainer.Pack(context.Background(), PackOptions{})
	require.NoError(t, err)
	assert.Equal(1, stats.BlobsDeferredOversized)
	assert.Zero(stats.PacksSealed)
	assert.FileExists(layout.LoosePath(hash))
}

func TestPackIncompleteReferenceInventoryPreservesLooseOrphans(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	catalog.referencesComplete = false
	liveContent := []byte("valid referenced content still packs")
	liveHash := writeMaintenanceLoose(t, layout, liveContent)
	catalog.addLoose(liveHash, layout.LoosePath(liveHash))
	orphanContent := []byte("unknown reachability must preserve this loose object")
	orphanHash := writeMaintenanceLoose(t, layout, orphanContent)
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	assert.True(stats.LooseOrphanSweepSuppressed)
	assert.FileExists(layout.LoosePath(orphanHash))
	assert.NoFileExists(layout.LoosePath(liveHash))
}

func TestPackIncompleteReferenceInventoryDefersOrphanPackReconciliation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	catalog.referencesComplete = false
	entry := buildStoreTestPack(t, layout, []byte("deferred orphan pack content"))
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})
	require.NoError(err)
	assert.Zero(stats.PacksAdopted)
	assert.Zero(stats.PacksRemoved)
	require.FileExists(layout.PackPath(entry.PackID))

	catalog.members[entry.Hash] = Reference{Hash: entry.Hash, OriginalHashes: []string{entry.Hash.String()}}
	catalog.referencesComplete = true
	stats, err = maintainer.Pack(context.Background(), PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.PacksAdopted)
	indexed, records := catalog.snapshot()
	assert.Equal(entry, indexed[entry.Hash])
	assert.Contains(records, entry.PackID)
}

func TestPackRotatesBeforeExceedingMaintenanceOutputLimits(t *testing.T) {
	for _, tt := range []struct {
		name   string
		limits func() Limits
	}{
		{name: "container bytes", limits: func() Limits {
			limits := DefaultLimits()
			limits.PackBytes = int64(pack.MinEntryOffset + 8 + 65 + 40)
			return limits
		}},
		{name: "footer bytes", limits: func() Limits {
			limits := DefaultLimits()
			limits.FooterBytes = 65
			return limits
		}},
		{name: "entry count", limits: func() Limits {
			limits := DefaultLimits()
			limits.PackEntries = 1
			return limits
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			layout := layoutForStoreTest(t)
			catalog := newMaintenanceCatalog()
			for _, content := range [][]byte{[]byte("aaaaaaaa"), []byte("bbbbbbbb"), []byte("cccccccc")} {
				hash := writeMaintenanceLoose(t, layout, content)
				catalog.addLoose(hash, layout.LoosePath(hash))
			}
			limits := tt.limits()
			maintainer := newMaintainerForTest(t, catalog, layout, limits)

			stats, err := maintainer.Pack(context.Background(), PackOptions{})
			require.NoError(err)
			assert.Equal(3, stats.PacksSealed)
			_, records := catalog.snapshot()
			require.Len(records, 3)
			for packID := range records {
				reader, openErr := OpenMaintenancePack(layout.PackPath(packID), limits)
				require.NoError(openErr)
				require.NoError(reader.Close())
			}
		})
	}
}

func TestPackDefersBlobThatCannotFitAnEmptyOutputPack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := []byte("eight888")
	hash := writeMaintenanceLoose(t, layout, content)
	catalog.addLoose(hash, layout.LoosePath(hash))
	limits := DefaultLimits()
	limits.PackBytes = int64(pack.MinEntryOffset + len(content) + 65 + 40 - 1)
	maintainer := newMaintainerForTest(t, catalog, layout, limits)

	stats, err := maintainer.Pack(context.Background(), PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.BlobsDeferredOversized)
	assert.Zero(stats.PacksSealed)
	assert.FileExists(layout.LoosePath(hash))
}

func TestPackSoftBudgetStopsAfterCommittedBlob(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	for _, content := range [][]byte{[]byte("first"), []byte("second")} {
		hash := writeMaintenanceLoose(t, layout, content)
		catalog.addLoose(hash, layout.LoosePath(hash))
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{MaxBytes: 1})
	require.NoError(t, err)
	assert.True(t, stats.BudgetExhausted)
	assert.Equal(t, 1, stats.BlobsPacked)
}

func TestPackPreservesCatalogCandidateOrder(t *testing.T) {
	require := require.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	contents := [][]byte{[]byte("first by locality"), []byte("second by locality"), []byte("third by locality")}
	want := make([]Hash, 0, len(contents))
	for _, content := range contents {
		hash := writeMaintenanceLoose(t, layout, content)
		catalog.addLoose(hash, layout.LoosePath(hash))
		want = append(want, hash)
	}
	sort.Slice(want, func(i, j int) bool { return want[i] > want[j] })
	catalog.setCandidateOrder(want)
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})
	require.NoError(err)
	require.Equal(1, stats.PacksSealed)
	_, packs := catalog.snapshot()
	require.Len(packs, 1)
	var packID string
	for id := range packs {
		packID = id
	}
	reader, err := pack.OpenReader(layout.PackPath(packID), nil)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reader.Close()) })
	entries := reader.Entries()
	require.Len(entries, len(want))
	got := make([]Hash, len(entries))
	for i, entry := range entries {
		got[i] = Hash(entry.ID.String())
	}
	require.Equal(want, got)
}

func TestPackCommitFailureLeavesRecoverableOrphanAndLooseSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := []byte("survive catalog commit failure")
	hash := writeMaintenanceLoose(t, layout, content)
	catalog.addLoose(hash, layout.LoosePath(hash))
	catalog.recordErr = errors.New("injected catalog failure")
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	_, err := maintainer.Pack(context.Background(), PackOptions{})
	require.ErrorContains(err, "injected catalog failure")
	assert.FileExists(layout.LoosePath(hash))
	orphans, err := filepath.Glob(filepath.Join(layout.PacksDir(), "*", "*"+PackExt))
	require.NoError(err)
	require.Len(orphans, 1)

	catalog.recordErr = nil
	stats, err := maintainer.Pack(context.Background(), PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.PacksRemoved, "the durable loose authority makes the failed pack redundant")
	assert.Equal(1, stats.PacksSealed)
	assert.NoFileExists(layout.LoosePath(hash))
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got)
}

func TestRepairDropsDanglingRecordsAndUnreferencedMappings(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	deadHash := hashForTest([]byte("dead"))
	catalog.entries[deadHash] = IndexEntry{Hash: deadHash, PackID: pack.NewPackID()}
	danglingID := pack.NewPackID()
	catalog.packs[danglingID] = PackRecord{PackID: danglingID, EntryCount: 1, CreatedAt: time.Now()}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, stats.RecordsDropped)
	assert.Equal(t, int64(1), stats.MappingsPruned)
}

func TestRepairRepacksValidLooseCopyWhenIndexedPackIsCorrupt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := []byte("recover from corrupt indexed pack")
	entry := buildStoreTestPack(t, layout, content)
	require.Equal(entry.Hash, writeMaintenanceLoose(t, layout, content))
	catalog.addLoose(entry.Hash, layout.LoosePath(entry.Hash))
	catalog.entries[entry.Hash] = entry
	catalog.packs[entry.PackID] = PackRecord{
		PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now(),
	}
	path := layout.PackPath(entry.PackID)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	var damaged [1]byte
	_, err = f.ReadAt(damaged[:], entry.Offset)
	require.NoError(err)
	damaged[0] ^= 0xff
	_, err = f.WriteAt(damaged[:], entry.Offset)
	require.NoError(err)
	require.NoError(f.Close())
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})
	require.NoError(err)
	assert.Equal(int64(1), stats.MappingsPruned)
	assert.Equal(1, stats.PacksSealed)
	assert.Equal(1, stats.BlobsPacked)
	indexed, _ := catalog.snapshot()
	require.Contains(indexed, entry.Hash)
	assert.NotEqual(entry.PackID, indexed[entry.Hash].PackID)
	assert.NoFileExists(layout.LoosePath(entry.Hash))
	got, _ := readStoreTest(t, maintainer.store, entry.Hash)
	assert.Equal(content, got)
}

func TestReconcileAdoptsOnlyFullyVerifiedOrphanPack(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		name := "valid"
		if damaged {
			name = "damaged"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			layout := layoutForStoreTest(t)
			catalog := newMaintenanceCatalog()
			content := []byte("orphan recovery content")
			entry := buildStoreTestPack(t, layout, content)
			catalog.members[entry.Hash] = Reference{Hash: entry.Hash, OriginalHashes: []string{entry.Hash.String()}}
			if damaged {
				path := layout.PackPath(entry.PackID)
				f, err := os.OpenFile(path, os.O_RDWR, 0)
				require.NoError(err)
				_, err = f.WriteAt([]byte{0xff}, entry.Offset)
				require.NoError(err)
				require.NoError(f.Close())
			}
			maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

			stats, err := maintainer.Pack(context.Background(), PackOptions{})
			require.NoError(err)
			entries, packs := catalog.snapshot()
			if damaged {
				assert.Equal(1, stats.PacksQuarantined)
				assert.Empty(entries)
				assert.Empty(packs)
				assert.FileExists(layout.PackPath(entry.PackID))
			} else {
				assert.Equal(1, stats.PacksAdopted)
				assert.Equal(entry, entries[entry.Hash])
				assert.Contains(packs, entry.PackID)
			}
		})
	}
}

func TestPackHonorsCancellationBeforeMutation(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := maintainer.Pack(ctx, PackOptions{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestPackDurablyCreatesPacksDirectory(t *testing.T) {
	require := require.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	var synced []string
	originalSyncDir := pack.SyncDir
	pack.SyncDir = func(path string) error {
		synced = append(synced, path)
		return nil
	}
	t.Cleanup(func() { pack.SyncDir = originalSyncDir })

	_, err := maintainer.Pack(context.Background(), PackOptions{})
	require.NoError(err)
	require.Contains(synced, layout.Root())
}

func newMaintainerForTest(t *testing.T, catalog Catalog, layout Layout, limits Limits) *Maintainer {
	t.Helper()
	maintainer, err := NewMaintainer(catalog, layout, MaintainerOptions{Limits: limits})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, maintainer.Close()) })
	return maintainer
}

func writeMaintenanceLoose(t *testing.T, layout Layout, content []byte) Hash {
	t.Helper()
	hash := hashForTest(content)
	path := layout.LoosePath(hash)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return hash
}
