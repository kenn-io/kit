package packstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

func TestPackMixedLooseRepresentationsUsesLogicalIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	contents := [][]byte{
		[]byte("raw packing candidate"),
		bytes.Repeat([]byte("compressed packing candidate\n"), 128),
		bytes.Repeat([]byte("duplicate physical representation\n"), 96),
	}
	hashes := make([]Hash, len(contents))

	hashes[0] = writeMaintenanceLoose(t, layout, contents[0])
	rawRelative, err := filepath.Rel(layout.Root(), layout.LoosePath(hashes[0]))
	require.NoError(err)
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hashes[0], Paths: []string{filepath.ToSlash(rawRelative)}, Size: int64(len(contents[0])),
	})

	hashes[1] = hashForTest(contents[1])
	writeCompressedLooseFixture(t, layout, hashes[1], int64(len(contents[1])), contents[1], nil)
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hashes[1], Paths: []string{layout.CompressedLoosePath(hashes[1])}, Size: int64(len(contents[1])),
	})

	hashes[2] = writeMaintenanceLoose(t, layout, contents[2])
	writeCompressedLooseFixture(t, layout, hashes[2], int64(len(contents[2])), contents[2], nil)
	compressedRelative, err := filepath.Rel(layout.Root(), layout.CompressedLoosePath(hashes[2]))
	require.NoError(err)
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hashes[2],
		Paths: []string{
			filepath.ToSlash(compressedRelative),
			layout.LoosePath(hashes[2]),
		},
		Size: int64(len(contents[2])),
	})

	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	stats, err := maintainer.Pack(context.Background(), PackOptions{})
	require.NoError(err)
	assert.Equal(len(contents), stats.BlobsPacked)
	assert.Equal(int64(len(contents[0])+len(contents[1])+len(contents[2])), stats.BytesPacked)

	indexed, records := catalog.snapshot()
	require.Len(records, 1)
	require.Len(indexed, len(contents), "one pack entry per logical hash")
	for index, hash := range hashes {
		entry, found := indexed[hash]
		require.True(found)
		assert.Equal(int64(len(contents[index])), entry.RawLen, "RawLen is decoded logical length")
		got, size := readStoreTest(t, maintainer.store, hash)
		assert.Equal(int64(len(contents[index])), size)
		assert.Equal(contents[index], got)
		assert.NoFileExists(layout.LoosePath(hash))
		assert.NoFileExists(layout.CompressedLoosePath(hash))
	}
}

func TestPackTreatsNoncanonicalHashZstdPathAsLegacyRaw(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := []byte("legacy raw bytes with a compressed-looking filename")
	hash := hashForTest(content)
	legacyRelative := filepath.ToSlash(filepath.Join("legacy", hash.String()+".zst"))
	legacyPath := filepath.Join(layout.Root(), filepath.FromSlash(legacyRelative))
	require.NoError(os.MkdirAll(filepath.Dir(legacyPath), 0o700))
	require.NoError(os.WriteFile(legacyPath, content, 0o600))
	catalog := newMaintenanceCatalog()
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hash, Paths: []string{legacyRelative}, Size: int64(len(content)),
	})
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	assert.NoFileExists(legacyPath)
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got)
}

func TestPackClassifiesWindowsCaseVariantCanonicalPathAsCompressed(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("case-insensitive canonical compressed path\n"), 32)
	hash := hashForTest(content)
	canonical := layout.CompressedLoosePath(hash)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	variant := filepath.Join(filepath.Dir(canonical), strings.ToUpper(filepath.Base(canonical)))
	require.NotEqual(canonical, variant)
	require.NoError(os.Rename(canonical, variant))
	originalEqual := canonicalLoosePathEqual
	canonicalLoosePathEqual = func(left, right string) bool {
		return canonicalLoosePathEqualForOS("windows", left, right)
	}
	t.Cleanup(func() { canonicalLoosePathEqual = originalEqual })
	catalog := newMaintenanceCatalog()
	addMaintenanceCandidate(catalog, Candidate{Hash: hash, Paths: []string{variant}, Size: int64(len(content))})
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got)
}

func TestCanonicalLoosePathEqualForOS(t *testing.T) {
	canonical := filepath.Join("root", "ab", "abcdef.zst")
	caseVariant := filepath.Join("ROOT", "AB", "ABCDEF.ZST")
	noncanonical := filepath.Join("root", "legacy", "abcdef.zst")

	assert.True(t, canonicalLoosePathEqualForOS("windows", canonical, caseVariant))
	assert.False(t, canonicalLoosePathEqualForOS("linux", canonical, caseVariant))
	assert.False(t, canonicalLoosePathEqualForOS("windows", canonical, noncanonical))
}

func TestPackMergesDuplicateCandidateFallbackPathsAndAliases(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("duplicate candidate fallback\n"), 64)
	hash := hashForTest(content)
	rawPath := layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(rawPath), 0o700))
	require.NoError(os.WriteFile(rawPath, bytes.Repeat([]byte{'x'}, len(content)), 0o600))
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	base := newMaintenanceCatalog()
	base.members[hash] = Reference{Hash: hash}
	catalog := &listedCandidateCatalog{
		maintenanceCatalog: base,
		listed: []Candidate{
			{
				Hash: hash, Size: int64(len(content)),
				Paths:          []string{filepath.ToSlash(filepath.Join("missing", hash.String())), rawPath},
				OriginalHashes: []string{"alias-a"},
			},
			{
				Hash: hash, Size: int64(len(content)),
				Paths:          []string{layout.CompressedLoosePath(hash)},
				OriginalHashes: []string{"alias-b", "alias-a"},
			},
		},
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	require.Len(catalog.recorded, 1)
	assert.Equal([]string{"alias-a", "alias-b"}, catalog.recorded[0].OriginalHashes)
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got)
}

func TestPackMergesAliasesAfterFirstSuccessfulDuplicateCandidate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("duplicate alias union\n"), 32)
	hash := writeMaintenanceLoose(t, layout, content)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	base := newMaintenanceCatalog()
	base.members[hash] = Reference{Hash: hash}
	catalog := &listedCandidateCatalog{
		maintenanceCatalog: base,
		listed: []Candidate{
			{Hash: hash, Size: int64(len(content)), Paths: []string{layout.LoosePath(hash)}, OriginalHashes: []string{"first"}},
			{Hash: hash, Size: int64(len(content)), Paths: []string{layout.CompressedLoosePath(hash)}, OriginalHashes: []string{"second"}},
		},
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	require.Len(catalog.recorded, 1)
	assert.Equal([]string{"first", "second"}, catalog.recorded[0].OriginalHashes)
}

func TestPackRejectsContradictoryDuplicateCandidateSizes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := []byte("contradictory candidate metadata")
	hash := writeMaintenanceLoose(t, layout, content)
	base := newMaintenanceCatalog()
	base.members[hash] = Reference{Hash: hash}
	catalog := &listedCandidateCatalog{
		maintenanceCatalog: base,
		listed: []Candidate{
			{Hash: hash, Size: int64(len(content)), Paths: []string{layout.LoosePath(hash)}},
			{Hash: hash, Size: int64(len(content)) + 1, Paths: []string{layout.LoosePath(hash)}},
		},
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsCorrupt)
	assert.Zero(stats.BlobsPacked)
	assert.FileExists(layout.LoosePath(hash))
	assert.Empty(catalog.recorded)
}

func TestPackRejectsCorruptCompressedCandidate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := bytes.Repeat([]byte("verify compressed candidate\n"), 64)
	hash := hashForTest(content)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, func(physical []byte) []byte {
		physical[len(physical)-1] ^= 0xff
		return physical
	})
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hash, Paths: []string{layout.CompressedLoosePath(hash)}, Size: int64(len(content)),
	})
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsCorrupt)
	assert.Zero(stats.BlobsPacked)
	assert.FileExists(layout.CompressedLoosePath(hash), "corrupt evidence is preserved")
}

func TestPackPreservesCompressedSourceReplacementAfterCatalogCommit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := bytes.Repeat([]byte("source replacement race\n"), 128)
	hash := hashForTest(content)
	path := layout.CompressedLoosePath(hash)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hash, Paths: []string{path}, Size: int64(len(content)),
	})
	replacement := []byte("replacement planted after catalog commit")
	catalog.commitHook = func() {
		require.NoError(os.Remove(path))
		require.NoError(os.WriteFile(path, replacement, 0o600))
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.ErrorIs(err, errIdentityChanged)
	assert.Equal(1, stats.BlobsPacked)
	assert.Equal(replacement, mustReadFile(t, path), "cleanup must not unlink a replacement inode")
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got)
}

func TestPackCancellationDuringCompressedCandidateCleansScratch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("cancel compressed candidate\n"), 4096)
	hash := hashForTest(content)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	catalog := newMaintenanceCatalog()
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hash, Paths: []string{layout.CompressedLoosePath(hash)}, Size: int64(len(content)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	originalReader := newLooseZstdReader
	newLooseZstdReader = func(src io.Reader) (looseZstdReader, error) {
		reader, err := originalReader(src)
		if err != nil {
			return nil, err
		}
		return &cancelAfterFirstLooseRead{looseZstdReader: reader, cancel: cancel}, nil
	}
	t.Cleanup(func() { newLooseZstdReader = originalReader })
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	_, err := maintainer.Pack(ctx, PackOptions{})

	require.ErrorIs(err, context.Canceled)
	assert.FileExists(layout.CompressedLoosePath(hash))
	indexed, records := catalog.snapshot()
	assert.Empty(indexed)
	assert.Empty(records)
	var scratch []string
	require.NoError(filepath.WalkDir(layout.PacksDir(), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			scratch = append(scratch, path)
		}
		return nil
	}))
	assert.Empty(scratch)
}

func TestPackPreservesDualCopiesWhenNeitherVerifies(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := []byte("expected dual-copy logical bytes")
	hash := hashForTest(content)
	rawPath := layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(rawPath), 0o700))
	require.NoError(os.WriteFile(rawPath, []byte("different dual-copy raw bytes"), 0o600))
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), []byte("different compressed bytes!!!!"), nil)
	compressedPath := layout.CompressedLoosePath(hash)
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hash, Paths: []string{compressedPath, rawPath}, Size: int64(len(content)),
	})
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsCorrupt)
	assert.Zero(stats.BlobsPacked)
	assert.FileExists(rawPath)
	assert.FileExists(compressedPath)
}

func TestPackPreservesSoleValidAndDiagnosticCopiesUntilAdoption(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := bytes.Repeat([]byte("sole valid loose representation\n"), 32)
	hash := writeMaintenanceLoose(t, layout, content)
	rawPath := layout.LoosePath(hash)
	compressedPath := layout.CompressedLoosePath(hash)
	corruptCompressed := []byte("corrupt compressed diagnostic evidence")
	require.NoError(os.WriteFile(compressedPath, corruptCompressed, 0o600))
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hash, Paths: []string{rawPath, compressedPath}, Size: int64(len(content)),
	})
	catalog.recordErr = errors.New("injected adoption failure")
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	_, err := maintainer.Pack(context.Background(), PackOptions{})
	require.ErrorContains(err, "injected adoption failure")
	assert.Equal(content, mustReadFile(t, rawPath), "the sole valid copy remains authoritative")
	assert.Equal(corruptCompressed, mustReadFile(t, compressedPath), "diagnostic evidence is preserved")

	catalog.recordErr = nil
	stats, err := maintainer.Pack(context.Background(), PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	assert.Equal(1, stats.BlobsCorrupt, "the invalid redundant copy is reported")
	assert.NoFileExists(rawPath, "the adopted pack now verifies as another logical representation")
	assert.Equal(corruptCompressed, mustReadFile(t, compressedPath), "corrupt diagnostic evidence remains")
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got)
}

func TestPackSweepsBothVerifiedLooseRepresentations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("redundant packed content\n"), 32)
	entry := buildStoreTestPack(t, layout, content)
	require.Equal(entry.Hash, writeMaintenanceLoose(t, layout, content))
	writeCompressedLooseFixture(t, layout, entry.Hash, int64(len(content)), content, nil)
	catalog := newMaintenanceCatalog()
	catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
	catalog.entries[entry.Hash] = entry
	catalog.packs[entry.PackID] = PackRecord{PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now()}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.NoError(err)
	assert.Equal(2, stats.LooseSwept)
	assert.NoFileExists(layout.LoosePath(entry.Hash))
	assert.NoFileExists(layout.CompressedLoosePath(entry.Hash))
}

func TestPackSweepReturnsRemovalFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := []byte("report redundant loose removal failure")
	entry := buildStoreTestPack(t, layout, content)
	require.Equal(entry.Hash, writeMaintenanceLoose(t, layout, content))
	catalog := newMaintenanceCatalog()
	catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
	catalog.entries[entry.Hash] = entry
	catalog.packs[entry.PackID] = PackRecord{PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now()}
	removeErr := errors.New("injected loose sweep removal failure")
	originalRemove := removeLooseCanonicalFile
	removeLooseCanonicalFile = func(path string) error {
		if filepath.Base(path) == "claimed" && strings.HasPrefix(
			filepath.Base(filepath.Dir(path)),
			"."+filepath.Base(layout.LoosePath(entry.Hash))+".remove-",
		) {
			return removeErr
		}
		return originalRemove(path)
	}
	t.Cleanup(func() { removeLooseCanonicalFile = originalRemove })
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	_, err := maintainer.Pack(context.Background(), PackOptions{})

	require.ErrorIs(err, removeErr)
	assert.FileExists(layout.LoosePath(entry.Hash))
}

func TestPackRepacksCompressedOnlyAuthorityWhenIndexedPackIsCorrupt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("compressed-only recovery authority\n"), 32)
	entry := buildStoreTestPack(t, layout, content)
	writeCompressedLooseFixture(t, layout, entry.Hash, int64(len(content)), content, nil)
	catalog := newMaintenanceCatalog()
	addMaintenanceCandidate(catalog, Candidate{
		Hash: entry.Hash, Paths: []string{layout.CompressedLoosePath(entry.Hash)}, Size: int64(len(content)),
	})
	catalog.entries[entry.Hash] = entry
	catalog.packs[entry.PackID] = PackRecord{
		PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now(),
	}
	path := layout.PackPath(entry.PackID)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	_, err = f.WriteAt([]byte{0xff}, entry.Offset)
	require.NoError(err)
	require.NoError(f.Close())
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.NoError(err)
	assert.Equal(int64(1), stats.MappingsPruned)
	assert.Equal(1, stats.BlobsPacked)
	got, _ := readStoreTest(t, maintainer.store, entry.Hash)
	assert.Equal(content, got)
}

func TestPackReconcilePreservesCompressedOnlyAuthorityOverOrphanPack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("compressed authority over orphan pack\n"), 32)
	entry := buildStoreTestPack(t, layout, content)
	writeCompressedLooseFixture(t, layout, entry.Hash, int64(len(content)), content, nil)
	catalog := newMaintenanceCatalog()
	catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.PacksRemoved)
	assert.Zero(stats.PacksAdopted)
	assert.NoFileExists(layout.PackPath(entry.PackID))
	assert.FileExists(layout.CompressedLoosePath(entry.Hash))
}

func TestPackOrphanSweepRecognizesOnlyCanonicalLooseNames(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	rawHash := writeMaintenanceLoose(t, layout, []byte("raw orphan"))
	compressedContent := []byte("compressed orphan")
	compressedHash := hashForTest(compressedContent)
	writeCompressedLooseFixture(t, layout, compressedHash, int64(len(compressedContent)), compressedContent, nil)
	unknown := layout.LoosePath(hashForTest([]byte("unknown extension"))) + ".bak"
	require.NoError(os.MkdirAll(filepath.Dir(unknown), 0o700))
	require.NoError(os.WriteFile(unknown, []byte("preserve"), 0o600))
	symlinkHash := hashForTest([]byte("symlink orphan"))
	symlinkPath := layout.CompressedLoosePath(symlinkHash)
	require.NoError(os.MkdirAll(filepath.Dir(symlinkPath), 0o700))
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(os.WriteFile(target, []byte("target"), 0o600))
	require.NoError(os.Symlink(target, symlinkPath))
	maintainer := newMaintainerForTest(t, newMaintenanceCatalog(), layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.NoError(err)
	assert.Equal(2, stats.LooseOrphansRemoved)
	assert.NoFileExists(layout.LoosePath(rawHash))
	assert.NoFileExists(layout.CompressedLoosePath(compressedHash))
	assert.FileExists(unknown)
	info, err := os.Lstat(symlinkPath)
	require.NoError(err)
	assert.NotZero(info.Mode() & os.ModeSymlink)
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

func newMaintainerForTest(t testing.TB, catalog Catalog, layout Layout, limits Limits) *Maintainer {
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

func addMaintenanceCandidate(catalog *maintenanceCatalog, candidate Candidate) {
	catalog.members[candidate.Hash] = Reference{
		Hash: candidate.Hash, OriginalHashes: []string{candidate.Hash.String()},
	}
	candidate.OriginalHashes = []string{candidate.Hash.String()}
	catalog.candidates[candidate.Hash] = candidate
}

type listedCandidateCatalog struct {
	*maintenanceCatalog
	listed   []Candidate
	recorded []Adoption
}

type cancelAfterFirstLooseRead struct {
	looseZstdReader
	cancel    context.CancelFunc
	cancelled bool
}

func (r *cancelAfterFirstLooseRead) Read(p []byte) (int, error) {
	n, err := r.looseZstdReader.Read(p)
	if n > 0 && !r.cancelled {
		r.cancelled = true
		r.cancel()
	}
	return n, err
}

func (c *listedCandidateCatalog) ListUnpacked(context.Context) ([]Candidate, error) {
	return append([]Candidate(nil), c.listed...), nil
}

func (c *listedCandidateCatalog) RecordPack(ctx context.Context, record PackRecord, adoptions []Adoption) error {
	c.recorded = make([]Adoption, len(adoptions))
	for index, adoption := range adoptions {
		c.recorded[index] = adoption
		c.recorded[index].OriginalHashes = append([]string(nil), adoption.OriginalHashes...)
	}
	return c.maintenanceCatalog.RecordPack(ctx, record, adoptions)
}
