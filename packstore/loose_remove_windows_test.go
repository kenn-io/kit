//go:build windows

package packstore

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWindowsLooseRemovalUnlinksActiveStream(t *testing.T) {
	layout := layoutForStoreTest(t)
	loose, err := NewLooseStore(layout)
	require.NoError(t, err)
	content := bytes.Repeat([]byte("active Windows loose reader\n"), 128)
	written, err := loose.WriteBytes(context.Background(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(t, err)
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		written.Hash: {Member: true},
	}}, layout)
	stream, size, err := store.OpenStream(context.Background(), written.Hash)
	require.NoError(t, err)
	require.Equal(t, int64(len(content)), size)
	t.Cleanup(func() { require.NoError(t, stream.Close()) })
	prefix := make([]byte, 37)
	_, err = io.ReadFull(stream, prefix)
	require.NoError(t, err)

	err = loose.Remove(written.Hash, BestEffortRemoval)

	require.NoError(t, err)
	assert.NoFileExists(t, written.Path)
	assertNoLooseRemovalClaims(t, written.Path)
	remainder, err := io.ReadAll(stream)
	require.NoError(t, err)
	assert.Equal(t, content, append(prefix, remainder...))
	require.NoError(t, stream.Verify())
}

func TestWindowsLooseCleanupRemovesClaimDirectories(t *testing.T) {
	t.Run("explicit removal", func(t *testing.T) {
		store := newLooseStoreForTest(t, StagingSameDirectory)
		written, err := store.WriteBytes(context.Background(), []byte("Windows explicit loose removal"), WriteOptions{
			Durability: AtomicPublication,
			Dedup:      VerifyFullHash,
		})
		require.NoError(t, err)

		err = store.Remove(written.Hash, BestEffortRemoval)

		require.NoError(t, err)
		assert.NoFileExists(t, written.Path)
		assertNoLooseRemovalClaims(t, written.Path)
	})

	t.Run("redundant sweep", func(t *testing.T) {
		layout := layoutForStoreTest(t)
		content := []byte("Windows redundant loose sweep")
		entry := buildStoreTestPack(t, layout, content)
		require.Equal(t, entry.Hash, writeMaintenanceLoose(t, layout, content))
		catalog := newMaintenanceCatalog()
		catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
		catalog.entries[entry.Hash] = entry
		catalog.packs[entry.PackID] = PackRecord{
			PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now(),
		}
		maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

		stats, err := maintainer.Pack(context.Background(), PackOptions{})

		require.NoError(t, err)
		assert.Equal(t, 1, stats.LooseSwept)
		assert.NoFileExists(t, layout.LoosePath(entry.Hash))
		assertNoLooseRemovalClaims(t, layout.LoosePath(entry.Hash))
	})

	t.Run("packed source", func(t *testing.T) {
		layout := layoutForStoreTest(t)
		content := []byte("Windows packed loose source")
		hash := writeMaintenanceLoose(t, layout, content)
		catalog := newMaintenanceCatalog()
		catalog.addLoose(hash, layout.LoosePath(hash))
		maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

		stats, err := maintainer.Pack(context.Background(), PackOptions{})

		require.NoError(t, err)
		assert.Equal(t, 1, stats.BlobsPacked)
		assert.NoFileExists(t, layout.LoosePath(hash))
		assertNoLooseRemovalClaims(t, layout.LoosePath(hash))
	})
}
