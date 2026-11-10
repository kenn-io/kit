package packstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

// seedOneEntryPacks seals one pack per content, catalogs each as a live
// member, and returns the entries in verification order.
func seedOneEntryPacks(t *testing.T, layout Layout, catalog *maintenanceCatalog, contents [][]byte, createdAt time.Time) []IndexEntry {
	t.Helper()
	entries := make([]IndexEntry, 0, len(contents))
	for i, content := range contents {
		entry := buildStoreTestPack(t, layout, content)
		catalog.members[entry.Hash] = Reference{Hash: entry.Hash, OriginalHashes: []string{entry.Hash.String()}}
		catalog.entries[entry.Hash] = entry
		catalog.packs[entry.PackID] = PackRecord{
			PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen,
			CreatedAt: createdAt.Add(time.Duration(i) * time.Minute),
		}
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, compareVerificationEntry)
	return entries
}

func repeatedContents(values ...byte) [][]byte {
	contents := make([][]byte, 0, len(values))
	for _, value := range values {
		contents = append(contents, bytes.Repeat([]byte{value}, 1024))
	}
	return contents
}

// cancelAfterVerificationCatalog cancels the pass at the first catalog step
// after verification, as a deadline expiring later in the pass would.
type cancelAfterVerificationCatalog struct {
	*maintenanceCatalog
	cancel context.CancelFunc
}

func (c *cancelAfterVerificationCatalog) PruneUnreferenced(ctx context.Context) (int64, error) {
	c.cancel()
	return c.maintenanceCatalog.PruneUnreferenced(ctx)
}

func TestPackVerificationResumesAfterPassCanceledLater(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	seedOneEntryPacks(t, layout, catalog, repeatedContents(1, 2, 3, 4), time.Now())
	ctx, cancel := context.WithCancel(t.Context())
	canceling := &cancelAfterVerificationCatalog{maintenanceCatalog: catalog, cancel: cancel}
	maintainer := newMaintainerForTest(t, canceling, layout, DefaultLimits())

	stats, err := maintainer.Pack(ctx, PackOptions{MaxVerifyBytes: 2048})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 2, stats.BlobsChecked)

	canceling.cancel = func() {}
	stats, err = maintainer.Pack(t.Context(), PackOptions{MaxVerifyBytes: 2048})
	require.NoError(t, err)
	assert.Equal(t, 2, stats.BlobsChecked)
	assert.False(t, stats.VerificationPending, "the retry must check the last two entries, not repeat the first two")
}

// cancelOnIndexDeleteCatalog cancels the pass while verification is under way.
type cancelOnIndexDeleteCatalog struct {
	*maintenanceCatalog
	cancel context.CancelFunc
}

func (c *cancelOnIndexDeleteCatalog) DeleteIndexEntry(ctx context.Context, hash Hash) error {
	c.cancel()
	return c.maintenanceCatalog.DeleteIndexEntry(ctx, hash)
}

func TestPackVerificationKeepsProgressMadeBeforeCancellation(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	entries := seedOneEntryPacks(t, layout, catalog, repeatedContents(1, 2, 3, 4), time.Now())
	// The second entry's pack is gone; dropping its mapping cancels the pass
	// before the third entry is examined.
	delete(catalog.packs, entries[1].PackID)
	require.NoError(t, os.Remove(layout.PackPath(entries[1].PackID)))
	ctx, cancel := context.WithCancel(t.Context())
	canceling := &cancelOnIndexDeleteCatalog{maintenanceCatalog: catalog, cancel: cancel}
	maintainer := newMaintainerForTest(t, canceling, layout, DefaultLimits())

	stats, err := maintainer.Pack(ctx, PackOptions{MaxVerifyBytes: 2048})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 2, stats.BlobsChecked)
	require.Equal(t, int64(1), stats.MappingsPruned)

	stats, err = maintainer.Pack(t.Context(), PackOptions{MaxVerifyBytes: 2048})
	require.NoError(t, err)
	assert.Equal(t, 2, stats.BlobsChecked)
	assert.False(t, stats.VerificationPending, "the retry must start after the dropped mapping")
}

func TestPackVerificationRevisitsUncommittedRepairButNotEarlierEntries(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	contents := repeatedContents(1, 2, 3, 4)
	entries := seedOneEntryPacks(t, layout, catalog, contents, time.Now())
	damaged := entries[1]
	var damagedContent []byte
	for _, content := range contents {
		if hashForTest(content) == damaged.Hash {
			damagedContent = content
		}
	}
	require.NotNil(t, damagedContent)
	writeMaintenanceLoose(t, layout, damagedContent)
	file, err := os.OpenFile(layout.PackPath(damaged.PackID), os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = file.WriteAt([]byte{0xff}, damaged.Offset)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	catalog.adoptErr = errors.New("catalog commit failed")
	stats, err := maintainer.Pack(t.Context(), PackOptions{MaxVerifyBytes: 3072})
	require.ErrorIs(t, err, catalog.adoptErr)
	require.Equal(t, 3, stats.BlobsChecked)
	catalog.adoptErr = nil

	stats, err = maintainer.Pack(t.Context(), PackOptions{MaxVerifyBytes: 3072})
	require.NoError(t, err)
	assert.Equal(t, 3, stats.BlobsChecked, "the retry starts at the uncommitted repair")
	assert.False(t, stats.VerificationPending)
	assert.Equal(t, 1, stats.BlobsPacked)
	repaired, _ := catalog.snapshot()
	assert.NotEqual(t, damaged.PackID, repaired[damaged.Hash].PackID)
	got, _ := readStoreTest(t, maintainer.store, damaged.Hash)
	assert.Equal(t, damagedContent, got)
}

func TestPackVerificationRevisitsRepairSkippedForRetryableFailure(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	contents := repeatedContents(1, 2, 3, 4)
	entries := seedOneEntryPacks(t, layout, catalog, contents, time.Now())
	damaged := entries[1]
	var damagedContent []byte
	for _, content := range contents {
		if hashForTest(content) == damaged.Hash {
			damagedContent = content
		}
	}
	require.NotNil(t, damagedContent)
	writeMaintenanceLoose(t, layout, damagedContent)
	file, err := os.OpenFile(layout.PackPath(damaged.PackID), os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = file.WriteAt([]byte{0xff}, damaged.Offset)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	maintainer.openVerificationPin = func(string) (identityPin, fs.FileInfo, error) {
		return nil, nil, errors.New("scratch disk full")
	}

	stats, err := maintainer.Pack(t.Context(), PackOptions{MaxVerifyBytes: 3072})
	require.NoError(t, err)
	require.Equal(t, 3, stats.BlobsChecked)
	require.Zero(t, stats.BlobsPacked, "the repair could not be packed")

	maintainer.openVerificationPin = openLooseMaintenanceVerificationPin
	stats, err = maintainer.Pack(t.Context(), PackOptions{MaxVerifyBytes: 3072})
	require.NoError(t, err)
	assert.Equal(t, 3, stats.BlobsChecked, "the retry starts at the skipped repair")
	assert.Equal(t, 1, stats.BlobsPacked)
	repaired, _ := catalog.snapshot()
	assert.NotEqual(t, damaged.PackID, repaired[damaged.Hash].PackID)
	got, _ := readStoreTest(t, maintainer.store, damaged.Hash)
	assert.Equal(t, damagedContent, got)
}

func TestPackExhaustiveVerificationKeepsRetryableRepairBeforeBoundedCursor(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	contents := repeatedContents(1, 2, 3, 4)
	entries := seedOneEntryPacks(t, layout, catalog, contents, time.Now())
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	// A bounded pass moves the saved cursor past the second entry.
	stats, err := maintainer.Pack(t.Context(), PackOptions{MaxVerifyBytes: 3072})
	require.NoError(t, err)
	require.Equal(t, 3, stats.BlobsChecked)

	damaged := entries[1]
	var damagedContent []byte
	for _, content := range contents {
		if hashForTest(content) == damaged.Hash {
			damagedContent = content
		}
	}
	require.NotNil(t, damagedContent)
	writeMaintenanceLoose(t, layout, damagedContent)
	file, err := os.OpenFile(layout.PackPath(damaged.PackID), os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = file.WriteAt([]byte{0xff}, damaged.Offset)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	maintainer.openVerificationPin = func(string) (identityPin, fs.FileInfo, error) {
		return nil, nil, errors.New("scratch disk full")
	}
	stats, err = maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 4, stats.BlobsChecked, "an exhaustive pass checks every entry")
	require.Zero(t, stats.BlobsPacked, "the repair could not be packed")

	maintainer.openVerificationPin = openLooseMaintenanceVerificationPin
	stats, err = maintainer.Pack(t.Context(), PackOptions{MaxVerifyBytes: 3072})
	require.NoError(t, err)
	assert.Equal(t, 1, stats.BlobsPacked, "the bounded pass must find the repair again")
	got, _ := readStoreTest(t, maintainer.store, damaged.Hash)
	assert.Equal(t, damagedContent, got)
}

// recordListingCatalog refuses per-pack record lookups.
type recordListingCatalog struct {
	*maintenanceCatalog
}

func (*recordListingCatalog) HasPackRecord(context.Context, string) (bool, error) {
	return false, errors.New("unexpected per-pack record lookup")
}

func TestPackReconcilesOrphansFromOneRecordListing(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	recorded := seedOneEntryPacks(t, layout, catalog, repeatedContents(1, 2), time.Now())
	orphan := buildStoreTestPack(t, layout, []byte("sealed before its record committed"))
	catalog.members[orphan.Hash] = Reference{Hash: orphan.Hash, OriginalHashes: []string{orphan.Hash.String()}}
	maintainer := newMaintainerForTest(t, &recordListingCatalog{maintenanceCatalog: catalog}, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, stats.PacksAdopted)
	entries, packs := catalog.snapshot()
	assert.Equal(t, orphan.PackID, entries[orphan.Hash].PackID)
	assert.Contains(t, packs, orphan.PackID)
	for _, entry := range recorded {
		assert.Equal(t, entry, entries[entry.Hash])
		assert.FileExists(t, layout.PackPath(entry.PackID))
	}
}

func TestPackHoldsSmallYoungLooseContent(t *testing.T) {
	missingRow := func(layout Layout, hashes []Hash) Candidate {
		return Candidate{Hash: hashes[0], Paths: []string{filepath.Join(layout.Root(), "missing-copy")}, Size: 1024}
	}
	oversizedRow := func(layout Layout, _ []Hash) Candidate {
		hash := hashForTest([]byte("larger than the blob limit"))
		return Candidate{Hash: hash, Paths: []string{layout.LoosePath(hash)}, Size: 1 << 40}
	}
	conflictingRow := func(layout Layout, hashes []Hash) Candidate {
		return Candidate{Hash: hashes[1], Paths: []string{layout.LoosePath(hashes[1])}, Size: 4096}
	}
	tests := []struct {
		name          string
		minSealBytes  int64
		maxLooseAge   time.Duration
		age           time.Duration
		removeLoose   bool
		relative      bool
		extraRow      func(Layout, []Hash) Candidate
		wantSealed    int
		wantDeferred  bool
		wantMissing   int
		wantOversized int
		wantCorrupt   int
	}{
		{name: "zero threshold seals every pass", wantSealed: 1},
		{name: "below threshold and young", minSealBytes: 4096, maxLooseAge: time.Hour, age: time.Minute, wantDeferred: true},
		{
			name: "store-relative paths below threshold and young", minSealBytes: 4096, maxLooseAge: time.Hour,
			age: time.Minute, relative: true, wantDeferred: true,
		},
		{
			name: "stale duplicate row below threshold and young", minSealBytes: 4096, maxLooseAge: time.Hour,
			age: time.Minute, extraRow: missingRow, wantDeferred: true,
		},
		{name: "threshold reached", minSealBytes: 2048, maxLooseAge: time.Hour, wantSealed: 1},
		{name: "oldest reaches age", minSealBytes: 4096, maxLooseAge: time.Hour, age: time.Hour, wantSealed: 1},
		{name: "default age not reached", minSealBytes: 4096, age: 23 * time.Hour, wantDeferred: true},
		{name: "default age reached", minSealBytes: 4096, age: 24 * time.Hour, wantSealed: 1},
		{
			name: "missing loose content is held and reported", minSealBytes: 4096, removeLoose: true,
			wantDeferred: true, wantMissing: 1,
		},
		{
			name: "oversized member is held and reported", minSealBytes: 4096, extraRow: oversizedRow,
			wantDeferred: true, wantOversized: 1,
		},
		{
			name: "conflicting sizes are held and reported", minSealBytes: 4096, extraRow: conflictingRow,
			wantDeferred: true, wantCorrupt: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			layout := layoutForStoreTest(t)
			catalog := newMaintenanceCatalog()
			var newest time.Time
			var hashes []Hash
			for _, content := range [][]byte{
				bytes.Repeat([]byte("a"), 1024), bytes.Repeat([]byte("b"), 1024),
			} {
				hash := writeMaintenanceLoose(t, layout, content)
				catalog.addLoose(hash, layout.LoosePath(hash))
				if tt.relative {
					relative, err := filepath.Rel(layout.Root(), layout.LoosePath(hash))
					require.NoError(t, err)
					candidate := catalog.candidates[hash]
					candidate.Paths = []string{filepath.ToSlash(relative)}
					catalog.candidates[hash] = candidate
				}
				info, err := os.Stat(layout.LoosePath(hash))
				require.NoError(t, err)
				if info.ModTime().After(newest) {
					newest = info.ModTime()
				}
				hashes = append(hashes, hash)
			}
			if tt.removeLoose {
				require.NoError(t, os.Remove(layout.LoosePath(hashes[0])))
			}
			var listing Catalog = catalog
			if tt.extraRow != nil {
				listing = &extraRowCatalog{maintenanceCatalog: catalog, before: []Candidate{tt.extraRow(layout, hashes)}}
			}
			maintainer := newMaintainerForTest(t, listing, layout, DefaultLimits())
			opts := PackOptions{MinSealBytes: tt.minSealBytes, MaxLooseAge: tt.maxLooseAge, Now: newest.Add(tt.age)}

			// A held pass must stay held when nothing changes, so it cannot
			// seal one small pack per pass.
			passes := 1
			if tt.wantDeferred {
				passes = 2
			}
			for range passes {
				stats, err := maintainer.Pack(t.Context(), opts)
				require.NoError(t, err)
				assert.Equal(t, tt.wantSealed, stats.PacksSealed)
				assert.Equal(t, tt.wantDeferred, stats.LooseSealDeferred)
				assert.Equal(t, tt.wantMissing, stats.BlobsMissing)
				assert.Equal(t, tt.wantOversized, stats.BlobsDeferredOversized)
				assert.Equal(t, tt.wantCorrupt, stats.BlobsCorrupt)
			}
			entries, _ := catalog.snapshot()
			if tt.wantDeferred {
				assert.Empty(t, entries)
				got, _ := readStoreTest(t, maintainer.store, hashes[1])
				assert.Equal(t, bytes.Repeat([]byte("b"), 1024), got)
			}
		})
	}
}

func TestPackRemembersLooseContentItRejected(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	now := time.Now()
	damaged := writeMaintenanceLoose(t, layout, bytes.Repeat([]byte("a"), 1024))
	catalog.addLoose(damaged, layout.LoosePath(damaged))
	// The truncated file still exists and is old, so only reading it shows
	// that packing cannot use it.
	require.NoError(t, os.Truncate(layout.LoosePath(damaged), 512))
	old := now.Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(layout.LoosePath(damaged), old, old))
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	opts := PackOptions{MinSealBytes: 4096, MaxLooseAge: time.Hour, Now: now}

	stats, err := maintainer.Pack(t.Context(), opts)
	require.NoError(t, err)
	require.False(t, stats.LooseSealDeferred, "the old file makes the first pass ready")
	require.Equal(t, 1, stats.BlobsCorrupt)
	require.Zero(t, stats.PacksSealed)

	young := writeMaintenanceLoose(t, layout, bytes.Repeat([]byte("b"), 1024))
	require.NoError(t, os.Chtimes(layout.LoosePath(young), now, now))
	catalog.addLoose(young, layout.LoosePath(young))
	for range 2 {
		stats, err = maintainer.Pack(t.Context(), opts)
		require.NoError(t, err)
		assert.True(t, stats.LooseSealDeferred, "the rejected file must not seal the young one")
		assert.Zero(t, stats.PacksSealed)
		assert.Equal(t, 1, stats.BlobsCorrupt)
	}
	entries, _ := catalog.snapshot()
	assert.Empty(t, entries)

	loose, err := NewLooseStore(layout)
	require.NoError(t, err)
	_, err = loose.Repair(t.Context(), bytes.NewReader(bytes.Repeat([]byte("a"), 1024)),
		LooseIdentity{Hash: damaged, Size: 1024}, RepairOptions{Durability: DurablePublication})
	require.NoError(t, err)
	// Both members together now reach the threshold, but only if the repaired
	// one counts again.
	opts.MinSealBytes = 2048
	stats, err = maintainer.Pack(t.Context(), opts)
	require.NoError(t, err)
	assert.False(t, stats.LooseSealDeferred, "the repair must make the member eligible again")
	assert.Zero(t, stats.BlobsCorrupt)
	assert.Equal(t, 2, stats.BlobsPacked)
	got, _ := readStoreTest(t, maintainer.store, damaged)
	assert.Equal(t, bytes.Repeat([]byte("a"), 1024), got)
}

// extraRowCatalog lists extra unpacked rows around the catalog's own.
type extraRowCatalog struct {
	*maintenanceCatalog
	before []Candidate
	after  []Candidate
}

func (c *extraRowCatalog) ListUnpacked(ctx context.Context) ([]Candidate, error) {
	candidates, err := c.maintenanceCatalog.ListUnpacked(ctx)
	return slices.Concat(c.before, candidates, c.after), err
}

func TestPackRetriesLooseContentAfterOperationalFailure(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	first := writeMaintenanceLoose(t, layout, bytes.Repeat([]byte("a"), 1024))
	require.NoError(t, os.Chtimes(layout.LoosePath(first), old, old))
	catalog.addLoose(first, layout.LoosePath(first))
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	maintainer.openVerificationPin = func(string) (identityPin, fs.FileInfo, error) {
		return nil, nil, errors.New("scratch disk full")
	}
	opts := PackOptions{MinSealBytes: 4096, MaxLooseAge: time.Hour, Now: now}

	stats, err := maintainer.Pack(t.Context(), opts)
	require.NoError(t, err)
	require.Equal(t, 1, stats.BlobsCorrupt)
	require.Zero(t, stats.PacksSealed)

	maintainer.openVerificationPin = openLooseMaintenanceVerificationPin
	second := writeMaintenanceLoose(t, layout, bytes.Repeat([]byte("b"), 1024))
	require.NoError(t, os.Chtimes(layout.LoosePath(second), now, now))
	catalog.addLoose(second, layout.LoosePath(second))
	// Both members together reach the threshold only if the first still counts.
	opts.MinSealBytes = 2048
	stats, err = maintainer.Pack(t.Context(), opts)
	require.NoError(t, err)
	assert.False(t, stats.LooseSealDeferred, "a failure unrelated to the content must not hide the member")
	assert.Equal(t, 2, stats.BlobsPacked)
}

// failingReader fails every read, as a disk that returns I/O errors would.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("input/output error") }

func TestPackRetriesCompressedLooseContentAfterReadFailure(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	loose, err := NewLooseStore(layout)
	require.NoError(t, err)
	content := bytes.Repeat([]byte("a"), 4096)
	written, err := loose.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	})
	require.NoError(t, err)
	compressed := layout.CompressedLoosePath(written.Hash)
	require.Equal(t, compressed, written.Path, "the test needs a compressed loose object")
	require.NoError(t, os.Chtimes(compressed, old, old))
	addMaintenanceCandidate(catalog, Candidate{Hash: written.Hash, Paths: []string{compressed}, Size: 4096})
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	// The header reads fine; the compressed payload after it does not.
	original := readLooseFile
	opened := 0
	readLooseFile = func(file *os.File) io.Reader {
		opened++
		if opened%2 == 0 {
			return failingReader{}
		}
		return file
	}
	t.Cleanup(func() { readLooseFile = original })
	opts := PackOptions{MinSealBytes: 8192, MaxLooseAge: time.Hour, Now: now}

	stats, err := maintainer.Pack(t.Context(), opts)
	require.NoError(t, err)
	require.Equal(t, 1, stats.BlobsCorrupt)
	require.Zero(t, stats.BlobsPacked)

	readLooseFile = original
	young := writeMaintenanceLoose(t, layout, bytes.Repeat([]byte("b"), 4096))
	require.NoError(t, os.Chtimes(layout.LoosePath(young), now, now))
	catalog.addLoose(young, layout.LoosePath(young))
	// Both members together reach the threshold only if the first still counts.
	stats, err = maintainer.Pack(t.Context(), opts)
	require.NoError(t, err)
	assert.False(t, stats.LooseSealDeferred, "a read failure must not hide the unchanged file")
	assert.Equal(t, 2, stats.BlobsPacked)
	got, _ := readStoreTest(t, maintainer.store, written.Hash)
	assert.Equal(t, content, got)
}

func TestPackRechecksRejectedLooseContentWhenACopyAppears(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	content := bytes.Repeat([]byte("a"), 1024)
	damaged := writeMaintenanceLoose(t, layout, content)
	require.NoError(t, os.Truncate(layout.LoosePath(damaged), 512))
	require.NoError(t, os.Chtimes(layout.LoosePath(damaged), old, old))
	alternate := filepath.Join(layout.Root(), "alternate-copy")
	catalog.addLoose(damaged, layout.LoosePath(damaged))
	candidate := catalog.candidates[damaged]
	candidate.Paths = append(candidate.Paths, alternate)
	candidate.Size = 1024
	catalog.candidates[damaged] = candidate
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	opts := PackOptions{MinSealBytes: 4096, MaxLooseAge: time.Hour, Now: now}

	stats, err := maintainer.Pack(t.Context(), opts)
	require.NoError(t, err)
	require.Equal(t, 1, stats.BlobsCorrupt)

	require.NoError(t, os.WriteFile(alternate, content, 0o600))
	young := writeMaintenanceLoose(t, layout, bytes.Repeat([]byte("b"), 1024))
	require.NoError(t, os.Chtimes(layout.LoosePath(young), now, now))
	catalog.addLoose(young, layout.LoosePath(young))
	opts.MinSealBytes = 2048
	stats, err = maintainer.Pack(t.Context(), opts)
	require.NoError(t, err)
	assert.False(t, stats.LooseSealDeferred, "the new copy must make the member eligible again")
	assert.Equal(t, 2, stats.BlobsPacked)
	got, _ := readStoreTest(t, maintainer.store, damaged)
	assert.Equal(t, content, got)
}

func TestPackRechecksConflictingRowsEachPass(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	conflicted := writeMaintenanceLoose(t, layout, bytes.Repeat([]byte("a"), 1024))
	catalog.addLoose(conflicted, layout.LoosePath(conflicted))
	trigger := writeMaintenanceLoose(t, layout, bytes.Repeat([]byte("c"), 1024))
	require.NoError(t, os.Chtimes(layout.LoosePath(trigger), old, old))
	catalog.addLoose(trigger, layout.LoosePath(trigger))
	// The conflicting row comes after the real one, so the first size stays
	// the same when it is removed.
	listing := &extraRowCatalog{maintenanceCatalog: catalog, after: []Candidate{
		{Hash: conflicted, Paths: []string{layout.LoosePath(conflicted)}, Size: 4096},
	}}
	maintainer := newMaintainerForTest(t, listing, layout, DefaultLimits())
	opts := PackOptions{MinSealBytes: 4096, MaxLooseAge: time.Hour, Now: now}

	stats, err := maintainer.Pack(t.Context(), opts)
	require.NoError(t, err)
	require.Equal(t, 1, stats.BlobsCorrupt, "packing runs for the old member and skips the conflict")
	require.Equal(t, 1, stats.BlobsPacked)

	listing.after = nil
	opts.MinSealBytes = 1024
	stats, err = maintainer.Pack(t.Context(), opts)
	require.NoError(t, err)
	assert.False(t, stats.LooseSealDeferred, "the catalog fix must make the member eligible again")
	assert.Equal(t, 1, stats.BlobsPacked)
}

func TestLooseSealReadinessStopsWhenCanceled(t *testing.T) {
	layout := layoutForStoreTest(t)
	hash := writeMaintenanceLoose(t, layout, bytes.Repeat([]byte("a"), 1024))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	maintainer := newMaintainerForTest(t, newMaintenanceCatalog(), layout, DefaultLimits())

	_, err := maintainer.looseReadyToSeal(ctx, []Candidate{
		{Hash: hash, Paths: []string{layout.LoosePath(hash)}, Size: 1024},
	}, PackOptions{MinSealBytes: 4096, MaxLooseAge: time.Hour}, &PackStats{})
	require.ErrorIs(t, err, context.Canceled)
	_, err = statLoose(ctx, layout.Root(), candidateGroup{
		Hash: hash, Paths: []string{"missing-a", "missing-b", layout.LoosePath(hash)}, Size: 1024,
	})
	require.ErrorIs(t, err, context.Canceled)
}

func noiseContents(count int) [][]byte {
	contents := make([][]byte, 0, count)
	for i := range count {
		content := deterministicLooseNoise(1024)
		content[0] = byte(i)
		contents = append(contents, content)
	}
	return contents
}

func TestRepackMergesOldSmallPacksWithinBudget(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	now := time.Now()
	contents := noiseContents(8)
	old := now.Add(-48 * time.Hour)
	// Five old one-entry packs, created a minute apart.
	smalls := make([]IndexEntry, 0, 5)
	for i, content := range contents[:5] {
		smalls = append(smalls, seedOneEntryPacks(t, layout, catalog, [][]byte{content}, old.Add(time.Duration(i)*time.Hour))...)
	}
	for _, entry := range smalls {
		require.Equal(t, int64(1024), entry.StoredLen, "incompressible content keeps group sizes exact")
	}
	recent := seedOneEntryPacks(t, layout, catalog, [][]byte{contents[5]}, now)[0]
	writer, err := pack.NewWriter(t.TempDir(), pack.WriterOptions{})
	require.NoError(t, err)
	for _, content := range contents[6:] {
		_, err = writer.Append(content)
		require.NoError(t, err)
	}
	largeID := writer.ID()
	sealed, err := writer.Seal(layout.PackPath(largeID))
	require.NoError(t, err)
	large := PackRecord{PackID: largeID, EntryCount: 2, CreatedAt: old}
	for _, entry := range sealed {
		indexed := indexFromPack(largeID, entry)
		large.StoredBytes += indexed.StoredLen
		catalog.members[indexed.Hash] = Reference{Hash: indexed.Hash}
		catalog.entries[indexed.Hash] = indexed
	}
	catalog.packs[largeID] = large
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	opts := RepackOptions{TargetSize: 2500, MaxBytes: 2048, Now: now}

	stats, err := maintainer.Repack(t.Context(), opts)
	require.NoError(t, err)
	assert.Zero(t, stats.PacksMerged, "merging is opt-in")

	opts.Selection.MergeBelowStored = 2000
	// Each group fits two 1 KiB entries under the 2500-byte target; the budget
	// admits one group per run, oldest first. The fifth pack has no partner.
	for run, pair := range [][]IndexEntry{smalls[0:2], smalls[2:4]} {
		stats, err = maintainer.Repack(t.Context(), opts)
		require.NoError(t, err)
		assert.Equal(t, 2, stats.PacksMerged)
		assert.Equal(t, 1, stats.PacksSealed)
		assert.Equal(t, 2, stats.PacksRemoved)
		assert.Equal(t, int64(2048), stats.BytesRepacked)
		assert.Equal(t, run == 0, stats.BudgetExhausted)
		for _, entry := range pair {
			assert.NoFileExists(t, layout.PackPath(entry.PackID))
		}
	}
	stats, err = maintainer.Repack(t.Context(), opts)
	require.NoError(t, err)
	assert.Zero(t, stats.PacksMerged, "a lone old pack, a recent pack and a large pack stay")

	entries, packs := catalog.snapshot()
	assert.Len(t, packs, 5)
	for _, kept := range []IndexEntry{smalls[4], recent} {
		assert.Equal(t, kept.PackID, entries[kept.Hash].PackID)
	}
	for _, content := range contents[6:] {
		assert.Equal(t, largeID, entries[hashForTest(content)].PackID)
	}
	merged := map[string]int{}
	for _, entry := range smalls[:4] {
		merged[entries[entry.Hash].PackID]++
	}
	assert.Equal(t, []int{2, 2}, slices.Sorted(maps.Values(merged)))
	for _, content := range contents {
		got, _ := readStoreTest(t, maintainer.store, hashForTest(content))
		assert.Equal(t, content, got)
	}
}

func TestRepackMergeLeavesOutSourceWithCorruptPayload(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	now := time.Now()
	contents := noiseContents(3)
	sources := seedOneEntryPacks(t, layout, catalog, contents, now.Add(-48*time.Hour))
	damaged := sources[1]
	// The footer stays intact, so only reading the payload finds the damage.
	file, err := os.OpenFile(layout.PackPath(damaged.PackID), os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = file.WriteAt([]byte{0xff}, damaged.Offset)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Repack(t.Context(), RepackOptions{
		TargetSize: 4096, MaxBytes: 4096, Now: now, Selection: RepackSelection{MergeBelowStored: 2000},
	})
	require.Error(t, err)
	require.True(t, isSourceContentError(err), "the run reports the damaged source: %v", err)
	require.ErrorContains(t, err, damaged.PackID)
	assert.Equal(t, 2, stats.PacksMerged)
	assert.Equal(t, 1, stats.PacksSealed)
	entries, packs := catalog.snapshot()
	assert.Equal(t, damaged, entries[damaged.Hash])
	assert.Contains(t, packs, damaged.PackID)
	assert.FileExists(t, layout.PackPath(damaged.PackID))
	merged := entries[sources[0].Hash].PackID
	assert.NotEqual(t, sources[0].PackID, merged)
	assert.Equal(t, merged, entries[sources[2].Hash].PackID)
	for _, healthy := range []IndexEntry{sources[0], sources[2]} {
		assert.NoFileExists(t, layout.PackPath(healthy.PackID))
		content := contents[slices.IndexFunc(contents, func(c []byte) bool { return hashForTest(c) == healthy.Hash })]
		got, _ := readStoreTest(t, maintainer.store, healthy.Hash)
		assert.Equal(t, content, got)
	}
}

func TestRepeatedMaintenanceConvergesOnManySmallPacks(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	start := time.Now()
	contents := noiseContents(24)
	seedOneEntryPacks(t, layout, catalog, contents, start.Add(-48*time.Hour))
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	initial := len(catalog.packs)
	merged := 0
	for pass := range 24 {
		// New content keeps arriving in small writes between passes.
		content := deterministicLooseNoise(512)
		content[0], content[1] = 0xee, byte(pass)
		// Each pass happens twelve hours after the previous one.
		now := start.Add(time.Duration(pass) * 12 * time.Hour)
		hash := writeMaintenanceLoose(t, layout, content)
		require.NoError(t, os.Chtimes(layout.LoosePath(hash), now, now))
		catalog.addLoose(hash, layout.LoosePath(hash))
		contents = append(contents, content)
		_, err := maintainer.Pack(t.Context(), PackOptions{
			TargetSize: 4096, MaxBytes: 4096, MaxVerifyBytes: 4096, MinSealBytes: 2048, Now: now,
		})
		require.NoError(t, err)
		stats, err := maintainer.Repack(t.Context(), RepackOptions{
			TargetSize: 4096, MaxBytes: 4096, Now: now, Selection: RepackSelection{MergeBelowStored: 2048},
		})
		require.NoError(t, err)
		merged += stats.PacksMerged
		_, packs := catalog.snapshot()
		require.LessOrEqual(t, len(packs), initial, "pass %d grew the pack count", pass)
	}
	_, packs := catalog.snapshot()
	// 24 KiB of old content and 12 KiB of new content fill nine 4 KiB packs.
	assert.LessOrEqual(t, len(packs), 12)
	assert.Positive(t, merged)
	for _, content := range contents {
		got, _ := readStoreTest(t, maintainer.store, hashForTest(content))
		assert.Equal(t, content, got)
	}
}

// misattributedLiveCatalog lists live entries under a pack they do not name.
type misattributedLiveCatalog struct {
	*maintenanceCatalog
	packID string
}

func (c *misattributedLiveCatalog) ListLivePackEntries(ctx context.Context, packID string) ([]IndexEntry, error) {
	entries, err := c.maintenanceCatalog.ListLivePackEntries(ctx, packID)
	for i := range entries {
		entries[i].PackID = c.packID
	}
	return entries, err
}

func TestRepackRejectsLiveEntriesNamingAnotherPack(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	live, deadA, deadB := buildSparsePack(t, layout)
	catalog.members[live.Hash] = Reference{Hash: live.Hash}
	catalog.entries[live.Hash] = live
	catalog.entries[deadA.Hash] = deadA
	catalog.entries[deadB.Hash] = deadB
	catalog.packs[live.PackID] = PackRecord{
		PackID: live.PackID, EntryCount: 3,
		StoredBytes: live.StoredLen + deadA.StoredLen + deadB.StoredLen, CreatedAt: time.Now().Add(-48 * time.Hour),
	}
	misattributed := &misattributedLiveCatalog{maintenanceCatalog: catalog, packID: pack.NewPackID()}
	maintainer := newMaintainerForTest(t, misattributed, layout, DefaultLimits())

	_, err := maintainer.Repack(t.Context(), RepackOptions{
		Selection: RepackSelection{MinAge: time.Nanosecond, MinDeadStored: 1},
	})
	require.ErrorIs(t, err, pack.ErrCorrupt)
	entries, _ := catalog.snapshot()
	assert.Equal(t, live, entries[live.Hash])
	assert.FileExists(t, layout.PackPath(live.PackID))
}
