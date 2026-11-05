package packstore

import (
	"context"
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
