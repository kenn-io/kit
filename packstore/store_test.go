package packstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestStoreReadsOnlyCatalogMembersFromLooseAndPackedStorage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	loose := []byte("loose bytes")
	looseHash := hashForTest(loose)
	require.NoError(os.MkdirAll(filepath.Dir(layout.LoosePath(looseHash)), 0o700))
	require.NoError(os.WriteFile(layout.LoosePath(looseHash), loose, 0o600))
	packed := []byte("packed bytes")
	entry := buildStoreTestPack(t, layout, packed)
	packedHash := entry.Hash
	resolver := &mapResolver{locations: map[Hash]Location{
		looseHash:  {Member: true},
		packedHash: {Member: true, Pack: &entry},
	}}
	store := newStoreForTest(t, resolver, layout)

	got, size := readStoreTest(t, store, looseHash)
	assert.Equal(loose, got)
	assert.Equal(int64(len(loose)), size)
	got, size = readStoreTest(t, store, packedHash)
	assert.Equal(packed, got)
	assert.Equal(int64(len(packed)), size)

	resolver.locations[looseHash] = Location{}
	_, _, err := store.Open(context.Background(), looseHash)
	assert.ErrorIs(err, fs.ErrNotExist)
}

func TestStoreOpenReadsAndSeeksCompressedLooseContent(t *testing.T) {
	content := bytes.Repeat([]byte("seekable compressed content "), 1024)
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		hash: {Member: true},
	}}, layout)

	reader, size, err := store.Open(context.Background(), hash)
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), size)
	named, ok := reader.(interface{ Name() string })
	require.True(t, ok, "compressed compatibility reader must expose its private temporary path")
	temporaryPath := named.Name()
	assert.FileExists(t, temporaryPath)
	temporaryInfo, err := os.Stat(temporaryPath)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o600), temporaryInfo.Mode().Perm())

	offset, err := reader.Seek(9, io.SeekStart)
	require.NoError(t, err)
	assert.Equal(t, int64(9), offset)
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, content[9:], got)
	require.NoError(t, reader.Close())
	assert.NoFileExists(t, temporaryPath)
	require.NoError(t, reader.Close())
}

func TestStoreOpenRejectsCorruptCompressedLooseAndCleansTemporaryFile(t *testing.T) {
	content := bytes.Repeat([]byte("verify before seekable exposure "), 1024)
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	writeCompressedLooseFixture(
		t, layout, hash, int64(len(content)), bytes.Repeat([]byte{'x'}, len(content)), nil,
	)
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		hash: {Member: true},
	}}, layout)
	pattern := filepath.Join(os.TempDir(), "packstore-loose-open-*")
	before, err := filepath.Glob(pattern)
	require.NoError(t, err)

	reader, size, err := store.Open(context.Background(), hash)
	require.ErrorIs(t, err, ErrContentMismatch)
	assert.Nil(t, reader)
	assert.Zero(t, size)
	after, globErr := filepath.Glob(pattern)
	require.NoError(t, globErr)
	assert.ElementsMatch(t, before, after, "failed compatibility opens must remove private temporary files")
}

func TestStoreOpenTemporaryWriteFailureDoesNotDrainCompressedSource(t *testing.T) {
	content := bytes.Repeat([]byte("do not drain after temporary write failure\n"), 4096)
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		hash: {Member: true},
	}}, layout)

	originalReader := newLooseZstdReader
	var decodedBytes int64
	newLooseZstdReader = func(src io.Reader) (looseZstdReader, error) {
		reader, err := originalReader(src)
		if err != nil {
			return nil, err
		}
		return &countingLooseZstdReader{looseZstdReader: reader, read: &decodedBytes}, nil
	}
	t.Cleanup(func() { newLooseZstdReader = originalReader })

	originalCreate := createSeekableLooseTemp
	var temporaryPath string
	createSeekableLooseTemp = func() (*os.File, error) {
		file, err := os.CreateTemp(t.TempDir(), "failed-seekable-")
		if err != nil {
			return nil, err
		}
		temporaryPath = file.Name()
		return file, nil
	}
	t.Cleanup(func() { createSeekableLooseTemp = originalCreate })
	originalCopy := copySeekableLoose
	writeErr := errors.New("injected temporary write failure")
	copySeekableLoose = func(io.Writer, io.Reader, []byte) (int64, error) { return 0, writeErr }
	t.Cleanup(func() { copySeekableLoose = originalCopy })

	reader, _, err := store.Open(context.Background(), hash)
	require.ErrorIs(t, err, writeErr)
	assert.Nil(t, reader)
	assert.LessOrEqual(t, decodedBytes, int64(looseCopyBufferBytes))
	assert.NoFileExists(t, temporaryPath)
}

func TestStoreOpenDoesNotRetryTemporaryNotExist(t *testing.T) {
	content := []byte("temporary creation failure is not migration")
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	packed := buildStoreTestPack(t, layout, content)
	resolver := &sequenceResolver{locations: []Location{
		{Member: true},
		{Member: true, Pack: &packed},
	}}
	store := newStoreForTest(t, resolver, layout)
	stagingErr := fmt.Errorf("seekable staging unavailable: %w", fs.ErrNotExist)

	originalCreate := createSeekableLooseTemp
	createSeekableLooseTemp = func() (*os.File, error) { return nil, stagingErr }
	t.Cleanup(func() { createSeekableLooseTemp = originalCreate })

	reader, size, err := store.Open(context.Background(), hash)
	require.ErrorIs(t, err, stagingErr)
	assert.Nil(t, reader)
	assert.Zero(t, size)
	assert.Equal(t, 1, resolver.calls)
}

type countingLooseZstdReader struct {
	looseZstdReader
	read *int64
}

func (r *countingLooseZstdReader) Read(p []byte) (int, error) {
	n, err := r.looseZstdReader.Read(p)
	*r.read += int64(n)
	return n, err
}

func TestReadBoundedCompressedLooseParityAndHeaderPreflight(t *testing.T) {
	content := bytes.Repeat([]byte("bounded compressed content "), 1024)
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		hash: {Member: true},
	}}, layout)

	got, size, err := store.ReadBounded(context.Background(), hash, int64(len(content)))
	require.NoError(t, err)
	assert.Equal(t, content, got)
	assert.Equal(t, int64(len(content)), size)

	_, _, err = store.ReadBounded(context.Background(), hash, int64(len(content)-1))
	var limitErr *LimitError
	require.ErrorAs(t, err, &limitErr)
	assert.Equal(t, LimitBlobRawBytes, limitErr.Dimension)
	assert.Equal(t, uint64(len(content)), limitErr.Actual)
}

func TestReadBoundedPreflightsCompressedHeaderBeforeDecode(t *testing.T) {
	content := []byte("preflight identity")
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	path := layout.CompressedLoosePath(hash)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	header := encodeCompressedLooseHeader(1024)
	require.NoError(t, os.WriteFile(path, append(header[:], []byte("not zstd")...), 0o600))
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		hash: {Member: true},
	}}, layout)

	data, size, err := store.ReadBounded(context.Background(), hash, 16)
	var limitErr *LimitError
	require.ErrorAs(t, err, &limitErr)
	assert.Equal(t, LimitBlobRawBytes, limitErr.Dimension)
	assert.Equal(t, uint64(1024), limitErr.Actual)
	assert.Nil(t, data)
	assert.Zero(t, size)
}

func TestReadBoundedPreflightsPlatformIntBeforeAllocation(t *testing.T) {
	content := []byte("platform allocation preflight")
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	path := layout.CompressedLoosePath(hash)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	header := encodeCompressedLooseHeader(math.MaxInt64)
	require.NoError(t, os.WriteFile(path, append(header[:], []byte("not zstd")...), 0o600))
	limits := DefaultLimits()
	limits.BlobBytes = math.MaxInt64
	store, err := NewStore(&mapResolver{locations: map[Hash]Location{hash: {Member: true}}}, layout, StoreOptions{
		Limits: limits,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	originalMax := maxPlatformInt
	maxPlatformInt = 1024
	t.Cleanup(func() { maxPlatformInt = originalMax })

	data, size, err := store.ReadBounded(context.Background(), hash, math.MaxInt64)
	var limitErr *LimitError
	require.ErrorAs(t, err, &limitErr)
	assert.Equal(t, LimitBlobRawBytes, limitErr.Dimension)
	assert.Equal(t, uint64(math.MaxInt64), limitErr.Actual)
	assert.Equal(t, uint64(1024), limitErr.Limit)
	assert.Nil(t, data)
	assert.Zero(t, size)
}

func TestReadBoundedRejectsCorruptLooseContent(t *testing.T) {
	require := require.New(t)
	layout := layoutForStoreTest(t)
	content := []byte("expected loose bytes")
	hash := hashForTest(content)
	path := layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	corrupt := append([]byte(nil), content...)
	corrupt[0] ^= 0xff
	require.NoError(os.WriteFile(path, corrupt, 0o600))
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		hash: {Member: true},
	}}, layout)

	data, size, err := store.ReadBounded(context.Background(), hash, int64(len(content)))
	require.ErrorIs(err, ErrContentMismatch)
	require.Nil(data)
	require.Zero(size)
}

func TestStoreConstructorsRejectZeroLayout(t *testing.T) {
	t.Run("store", func(t *testing.T) {
		_, err := NewStore(&mapResolver{}, Layout{}, StoreOptions{})
		require.ErrorContains(t, err, "invalid empty layout")
	})
	t.Run("maintainer", func(t *testing.T) {
		_, err := NewMaintainer(newMaintenanceCatalog(), Layout{}, MaintainerOptions{})
		require.ErrorContains(t, err, "invalid empty layout")
	})
}

func TestStoreRetriesLooseToPackAndPackToLooseRacesOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := []byte("migration race")
	hash := hashForTest(content)
	entry := buildStoreTestPack(t, layout, content)
	loosePath := layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(loosePath), 0o700))
	require.NoError(os.WriteFile(loosePath, content, 0o600))

	looseToPack := &sequenceResolver{locations: []Location{{Member: true}, {Member: true, Pack: &entry}}}
	looseToPack.beforeFirstReturn = func() { require.NoError(os.Remove(loosePath)) }
	store := newStoreForTest(t, looseToPack, layout)
	got, _ := readStoreTest(t, store, hash)
	assert.Equal(content, got)
	assert.Equal(2, looseToPack.calls)
	require.NoError(store.Close())

	require.NoError(os.WriteFile(loosePath, content, 0o600))
	require.NoError(os.Remove(layout.PackPath(entry.PackID)))
	packToLoose := &sequenceResolver{locations: []Location{{Member: true, Pack: &entry}, {Member: true}}}
	store = newStoreForTest(t, packToLoose, layout)
	got, _ = readStoreTest(t, store, hash)
	assert.Equal(content, got)
	assert.Equal(2, packToLoose.calls)
}

func TestStoreRejectsForgedPackIndexMetadata(t *testing.T) {
	layout := layoutForStoreTest(t)
	content := []byte("metadata")
	entry := buildStoreTestPack(t, layout, content)
	entry.RawLen++
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		entry.Hash: {Member: true, Pack: &entry},
	}}, layout)

	_, _, err := store.Open(context.Background(), entry.Hash)
	require.ErrorContains(t, err, "metadata mismatch")
}

func TestStoreSharesBoundedAndOrdinaryCacheSlotsAndEvicts(t *testing.T) {
	require := require.New(t)
	layout := layoutForStoreTest(t)
	resolver := &mapResolver{locations: map[Hash]Location{}}
	store := newStoreForTest(t, resolver, layout)

	for i := range maxOpenReaders + 1 {
		content := []byte{byte(i), byte(i >> 8)}
		entry := buildStoreTestPack(t, layout, content)
		resolver.locations[entry.Hash] = Location{Member: true, Pack: &entry}
		r, _, err := store.Open(context.Background(), entry.Hash)
		require.NoError(err)
		require.NoError(r.Close())
		_, _, err = store.ReadBounded(context.Background(), entry.Hash, int64(len(content)))
		require.NoError(err)
		require.LessOrEqual(len(store.packReaders), maxOpenReaders)
	}
	assert.Len(t, store.order, maxOpenReaders)
	require.NoError(store.Close())
	assert.Empty(t, store.order)
}

func TestStoreReaderModeConversionPreservesOneCacheSlot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	entry := buildStoreTestPack(t, layout, []byte("one logical cache slot"))
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		entry.Hash: {Member: true, Pack: &entry},
	}}, layout)

	reader, _, err := store.Open(context.Background(), entry.Hash)
	require.NoError(err)
	require.NoError(reader.Close())
	assert.Equal([]string{entry.PackID}, store.order)

	_, _, err = store.ReadBounded(context.Background(), entry.Hash, entry.RawLen)
	require.NoError(err)
	assert.Equal([]string{entry.PackID}, store.order)

	reader, _, err = store.Open(context.Background(), entry.Hash)
	require.NoError(err)
	require.NoError(reader.Close())
	assert.Equal([]string{entry.PackID}, store.order)
}

func TestStoreConcurrentOrdinaryAndBoundedReads(t *testing.T) {
	require := require.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("concurrent packed read"), 4096)
	entry := buildStoreTestPack(t, layout, content)
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		entry.Hash: {Member: true, Pack: &entry},
	}}, layout)

	errs := make(chan error, 32)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			if i%2 == 0 {
				r, _, err := store.Open(context.Background(), entry.Hash)
				if err == nil {
					_, err = io.Copy(io.Discard, r)
					err = errors.Join(err, r.Close())
				}
				errs <- err
				return
			}
			got, _, err := store.ReadBounded(context.Background(), entry.Hash, int64(len(content)))
			if err == nil && !bytes.Equal(content, got) {
				err = errors.New("bounded content mismatch")
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(err)
	}
}

type mapResolver struct {
	locations map[Hash]Location
}

func (r *mapResolver) Resolve(_ context.Context, hash Hash) (Location, error) {
	return r.locations[hash], nil
}

type sequenceResolver struct {
	locations         []Location
	calls             int
	beforeFirstReturn func()
}

func (r *sequenceResolver) Resolve(_ context.Context, _ Hash) (Location, error) {
	if r.calls == 0 && r.beforeFirstReturn != nil {
		r.beforeFirstReturn()
	}
	index := min(r.calls, len(r.locations)-1)
	r.calls++
	return r.locations[index], nil
}

func layoutForStoreTest(t *testing.T) Layout {
	t.Helper()
	layout, err := NewLayout(t.TempDir(), LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(t, err)
	return layout
}

func newStoreForTest(t *testing.T, resolver Resolver, layout Layout) *Store {
	t.Helper()
	store, err := NewStore(resolver, layout, StoreOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func buildStoreTestPack(t *testing.T, layout Layout, content []byte) IndexEntry {
	t.Helper()
	staging := t.TempDir()
	w, err := pack.NewWriter(staging, pack.WriterOptions{})
	require.NoError(t, err)
	_, err = w.Append(content)
	require.NoError(t, err)
	packID := w.ID()
	require.NoError(t, os.MkdirAll(filepath.Dir(layout.PackPath(packID)), 0o700))
	entries, err := w.Seal(layout.PackPath(packID))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	entry := entries[0]
	hash, err := ParseHash(entry.ID.String())
	require.NoError(t, err)
	return IndexEntry{
		Hash: hash, PackID: packID, Offset: int64(entry.Offset),
		StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen),
		Flags: uint8(entry.Flags), CRC32C: entry.CRC32C,
	}
}

func readStoreTest(t *testing.T, store *Store, hash Hash) ([]byte, int64) {
	t.Helper()
	r, size, err := store.Open(context.Background(), hash)
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	return data, size
}
