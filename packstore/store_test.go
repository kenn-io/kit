package packstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
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
		require.LessOrEqual(len(store.readers)+len(store.boundedReaders), maxOpenReaders)
	}
	assert.Len(t, store.order, maxOpenReaders)
	require.NoError(store.Close())
	assert.Empty(t, store.order)
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
