//go:build unix

package packstore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const unreadableRemovalChild = "KIT_PACKSTORE_UNREADABLE_REMOVAL_CHILD"
const nonWritableShardPackingChild = "KIT_PACKSTORE_NONWRITABLE_SHARD_PACKING_CHILD"

func TestPackRetainsReadableLooseSourceWhenShardDeniesCleanup(t *testing.T) {
	if os.Getenv(nonWritableShardPackingChild) != "" {
		runNonWritableShardPackingChild(t)
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestPackRetainsReadableLooseSourceWhenShardDeniesCleanup$")
	command.Env = append(os.Environ(), nonWritableShardPackingChild+"=1")
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
}

func runNonWritableShardPackingChild(t *testing.T) {
	root, err := os.MkdirTemp("", "kit-packstore-nonwritable-shard-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	layout, err := NewLayout(root, LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(t, err)
	content := []byte("readable loose source in non-writable shard")
	hash := writeMaintenanceLoose(t, layout, content)
	path := layout.LoosePath(hash)
	shard := filepath.Dir(path)
	require.NoError(t, os.Chmod(shard, 0o500))
	dropUnreadableRemovalPrivileges(t, root)
	t.Cleanup(func() { require.NoError(t, os.Chmod(shard, 0o700)) })
	readable, err := os.Open(path)
	require.NoError(t, err, "fixture must remain readable")
	require.NoError(t, readable.Close())
	catalog := newMaintenanceCatalog()
	catalog.addLoose(hash, path)
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1, stats.BlobsPacked)
	assert.Zero(t, stats.BlobsCorrupt)
	assert.FileExists(t, path)
	assertNoLooseRemovalClaims(t, path)
	location, err := catalog.Resolve(context.Background(), hash)
	require.NoError(t, err)
	require.NotNil(t, location.Pack)
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(t, content, got)
}

func TestUnreadableLooseRemovalUsesIdentityOnlyPin(t *testing.T) {
	if mode := os.Getenv(unreadableRemovalChild); mode != "" {
		runUnreadableRemovalChild(t, mode)
		return
	}
	for _, mode := range []string{"explicit", "orphan", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestUnreadableLooseRemovalUsesIdentityOnlyPin$")
			command.Env = append(os.Environ(), unreadableRemovalChild+"="+mode)
			output, err := command.CombinedOutput()
			require.NoError(t, err, string(output))
		})
	}
}

func runUnreadableRemovalChild(t *testing.T, mode string) {
	root, err := os.MkdirTemp("", "kit-packstore-unreadable-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	layout, err := NewLayout(root, LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(t, err)
	content := fmt.Appendf(nil, "unreadable %s loose content", mode)
	hash := writeMaintenanceLoose(t, layout, content)
	path := layout.LoosePath(hash)
	require.NoError(t, os.Chmod(path, 0))
	dropUnreadableRemovalPrivileges(t, root)
	file, openErr := os.Open(path)
	if openErr == nil {
		require.NoError(t, file.Close())
	}
	require.Error(t, openErr, "fixture must genuinely deny ordinary read access")

	switch mode {
	case "explicit":
		store, err := NewLooseStore(layout)
		require.NoError(t, err)
		require.NoError(t, store.Remove(hash, BestEffortRemoval))
		assert.NoFileExists(t, path)
		assertNoIdentityPinDebris(t, path)
	case "orphan":
		catalog := newMaintenanceCatalog()
		maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
		stats, err := maintainer.Pack(context.Background(), PackOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, stats.LooseOrphansRemoved)
		assert.NoFileExists(t, path)
		assertNoIdentityPinDebris(t, path)
	case "replacement":
		replacement := []byte("unreadable raced replacement")
		held := path + ".held"
		originalHook := beforeLooseRemovalClaim
		var replacementIdentity os.FileInfo
		beforeLooseRemovalClaim = func(gotPath string) {
			if replacementIdentity != nil || filepath.Clean(gotPath) != filepath.Clean(path) {
				return
			}
			require.NoError(t, os.Rename(path, held))
			require.NoError(t, os.WriteFile(path, replacement, 0))
			replacementIdentity, err = os.Lstat(path)
			require.NoError(t, err)
		}
		t.Cleanup(func() { beforeLooseRemovalClaim = originalHook })
		store, err := NewLooseStore(layout)
		require.NoError(t, err)

		err = store.Remove(hash, BestEffortRemoval)

		require.ErrorIs(t, err, errIdentityChanged)
		require.NotNil(t, replacementIdentity)
		canonicalIdentity, statErr := os.Lstat(path)
		require.NoError(t, statErr)
		assert.True(t, os.SameFile(replacementIdentity, canonicalIdentity), "the exact unreadable replacement wins the removal race")
		assert.FileExists(t, held)
		assertNoLooseRemovalClaims(t, path)
		assertNoIdentityPinDebris(t, path)
	default:
		require.FailNow(t, "unknown unreadable-removal child mode", mode)
	}
}

func assertNoIdentityPinDebris(t *testing.T, canonical string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(
		filepath.Dir(canonical), "."+filepath.Base(canonical)+".pin-*",
	))
	require.NoError(t, err)
	assert.Empty(t, matches)
}

func dropUnreadableRemovalPrivileges(t *testing.T, root string) {
	t.Helper()
	if os.Geteuid() != 0 {
		return
	}
	const nobody = 65534
	require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return os.Chown(path, nobody, nobody)
	}))
	require.NoError(t, unix.Setgroups(nil))
	require.NoError(t, unix.Setgid(nobody))
	require.NoError(t, unix.Setuid(nobody))
	// Some kernels retain filesystem capability state until the credential
	// transition completes. A short stat boundary keeps the fixture ordering
	// explicit without depending on elapsed time.
	_, err := os.Stat(root)
	require.NoError(t, err)
}
