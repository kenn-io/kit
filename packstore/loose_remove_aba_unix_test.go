//go:build unix

package packstore

import (
	"context"
	"os"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
)

func TestLooseRemovePreservesExactSizeReplacementAtClaimBoundary(t *testing.T) {
	require := Require.New(t)
	layout := layoutForStoreTest(t)
	store, err := NewLooseStore(layout)
	require.NoError(err)
	content := []byte("original exact-size loose source")
	written, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash,
	})
	require.NoError(err)
	replacement := []byte("foreign! exact-size loose source")
	require.Len(replacement, len(content))
	installExactSizeRemovalReplacement(t, written.Path, replacement)

	err = store.Remove(written.Hash, BestEffortRemoval)

	require.ErrorIs(err, errIdentityChanged)
	Assert.Equal(t, replacement, mustReadFile(t, written.Path))
	assertNoLooseRemovalClaims(t, written.Path)
}

func TestPackSweepPreservesExactSizeReplacementAtClaimBoundary(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	layout := layoutForStoreTest(t)
	content := []byte("original exact-size sweep source")
	entry := buildStoreTestPack(t, layout, content)
	require.Equal(entry.Hash, writeMaintenanceLoose(t, layout, content))
	catalog := newMaintenanceCatalog()
	catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
	catalog.entries[entry.Hash] = entry
	catalog.packs[entry.PackID] = PackRecord{
		PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now(),
	}
	path := layout.LoosePath(entry.Hash)
	replacement := []byte("foreign! exact-size sweep source")
	require.Len(replacement, len(content))
	installExactSizeRemovalReplacement(t, path, replacement)
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.ErrorIs(err, errIdentityChanged)
	assert.Zero(stats.LooseSwept)
	assert.Equal(replacement, mustReadFile(t, path))
	assertNoLooseRemovalClaims(t, path)
}

func installExactSizeRemovalReplacement(t *testing.T, path string, replacement []byte) {
	t.Helper()
	originalHook := beforeLooseRemovalClaim
	triggered := false
	beforeLooseRemovalClaim = func(gotPath string) {
		if triggered || gotPath != path {
			return
		}
		triggered = true
		Require.NoError(t, os.Remove(path))
		Require.NoError(t, os.WriteFile(path, replacement, 0o600))
	}
	t.Cleanup(func() { beforeLooseRemovalClaim = originalHook })
	t.Cleanup(func() { Assert.True(t, triggered, "removal reached the exact-size replacement boundary") })
}
