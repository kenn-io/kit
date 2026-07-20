//go:build windows

package packstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
