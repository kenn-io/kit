package packstore

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSweepOrphansRemovesOnlyUnreferencedLooseContent(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	live := writeMaintenanceLoose(t, layout, []byte("referenced content"))
	catalog.addLoose(live, layout.LoosePath(live))
	orphan := writeMaintenanceLoose(t, layout, []byte("content nothing references"))
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.SweepOrphans(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, stats.LooseOrphansRemoved)
	assert.False(t, stats.Suppressed)
	assert.NoFileExists(t, layout.LoosePath(orphan))
	assert.FileExists(t, layout.LoosePath(live), "referenced content stays loose; sweeping does not pack it")
}

func TestSweepOrphansKeepsEverythingWhenReferencesAreIncomplete(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	catalog.referencesComplete = false
	orphan := writeMaintenanceLoose(t, layout, []byte("reachability unknown"))
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.SweepOrphans(t.Context())

	require.NoError(t, err)
	assert.True(t, stats.Suppressed)
	assert.Zero(t, stats.LooseOrphansRemoved)
	assert.FileExists(t, layout.LoosePath(orphan))
}

func TestSweepOrphansSucceedsBeforeTheStoreRootExists(t *testing.T) {
	layout, err := NewLayout(filepath.Join(t.TempDir(), "not-yet-written"), LayoutOptions{
		Staging: StagingStoreDirectory, StagingDir: "tmp",
	})
	require.NoError(t, err)
	maintainer := newMaintainerForTest(t, newMaintenanceCatalog(), layout, DefaultLimits())

	stats, err := maintainer.SweepOrphans(t.Context())

	require.NoError(t, err)
	assert.Equal(t, OrphanSweepStats{}, stats)
}

func TestSweepOrphansContinuesPastAnOrphanItCannotRemove(t *testing.T) {
	tests := []struct {
		name string
		deny func(t *testing.T, maintainer *Maintainer, blockedPath string)
	}{
		{
			name: "pin acquisition denied",
			deny: func(_ *testing.T, maintainer *Maintainer, blockedPath string) {
				maintainer.openIdentityPin = func(path string) (identityPin, fs.FileInfo, error) {
					if path == blockedPath {
						return nil, nil, fs.ErrPermission
					}
					return openLooseIdentityPin(path)
				}
			},
		},
		{
			name: "removal claim denied",
			deny: func(t *testing.T, _ *Maintainer, blockedPath string) {
				t.Helper()
				originalCreate := createLooseRemovalAside
				createLooseRemovalAside = func(path string) error {
					if strings.HasPrefix(filepath.Base(path), "."+filepath.Base(blockedPath)+".remove-") {
						return fs.ErrPermission
					}
					return originalCreate(path)
				}
				t.Cleanup(func() { createLooseRemovalAside = originalCreate })
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			layout := layoutForStoreTest(t)
			blocked := writeMaintenanceLoose(t, layout, []byte("orphan without removal permission"))
			removable := writeMaintenanceLoose(t, layout, []byte("orphan the sweep can remove"))
			maintainer := newMaintainerForTest(t, newMaintenanceCatalog(), layout, DefaultLimits())
			tt.deny(t, maintainer, layout.LoosePath(blocked))

			stats, err := maintainer.SweepOrphans(t.Context())

			require.NoError(t, err)
			assert.Equal(t, OrphanSweepStats{LooseOrphansRemoved: 1, LooseOrphansDeferred: 1}, stats)
			assert.FileExists(t, layout.LoosePath(blocked))
			assert.NoFileExists(t, layout.LoosePath(removable))
		})
	}
}

func TestSweepOrphansWaitsForInFlightWrites(t *testing.T) {
	layout := layoutForStoreTest(t)
	// A write publishes its file before the catalog references it.
	unreferenced := writeMaintenanceLoose(t, layout, []byte("published, not yet referenced"))
	synctest.Test(t, func(t *testing.T) {
		coordinator := NewCoordinator()
		maintainer, err := NewMaintainer(newMaintenanceCatalog(), layout, MaintainerOptions{Coordinator: coordinator, Limits: DefaultLimits()})
		require.NoError(t, err)
		defer func() { assert.NoError(t, maintainer.Close()) }()
		write, err := coordinator.AcquireMutation(t.Context())
		require.NoError(t, err)

		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		_, err = maintainer.SweepOrphans(ctx)

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.FileExists(t, layout.LoosePath(unreferenced))
		require.NoError(t, write.Release())
	})
}

func TestSweepOrphansStopsWithinAShardWhenCanceled(t *testing.T) {
	layout := layoutForStoreTest(t)
	first, second := sameShardContents(t)
	orphans := []Hash{writeMaintenanceLoose(t, layout, first), writeMaintenanceLoose(t, layout, second)}
	maintainer := newMaintainerForTest(t, newMaintenanceCatalog(), layout, DefaultLimits())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opened := 0
	maintainer.openIdentityPin = func(path string) (identityPin, fs.FileInfo, error) {
		opened++
		cancel()
		return openLooseIdentityPin(path)
	}

	stats, err := maintainer.SweepOrphans(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, opened)
	assert.Equal(t, 1, stats.LooseOrphansRemoved)
	remaining := 0
	for _, orphan := range orphans {
		if _, statErr := os.Stat(layout.LoosePath(orphan)); statErr == nil {
			remaining++
		}
	}
	assert.Equal(t, 1, remaining, "the orphan after cancellation stays in place")
}

// sameShardContents returns two contents whose hashes share a shard directory.
func sameShardContents(t *testing.T) ([]byte, []byte) {
	t.Helper()
	seen := map[string][]byte{}
	for i := 0; ; i++ {
		content := []byte(fmt.Sprintf("orphan %d", i))
		shard := hashForTest(content).String()[:2]
		if earlier, ok := seen[shard]; ok {
			return earlier, content
		}
		seen[shard] = content
	}
}
