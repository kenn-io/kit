package backup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHashMapCacheRejectsNonCanonicalRepoID pins that the cache functions
// refuse a repoID that is not a canonical generated repository ID: the ID
// becomes a filename under cacheDir, so a value carrying path separators
// could otherwise read or write files outside the cache directory.
func TestHashMapCacheRejectsNonCanonicalRepoID(t *testing.T) {
	require := require.New(t)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	m := &PageHashMap{PageSize: 4096, PageCount: 0}

	for _, id := range []string{"../escape", "sub/dir", "", "not-a-uuid"} {
		err := SaveHashMapCache(cacheDir, id, "snap", m)
		require.ErrorContains(err, "not a canonical repository ID", "save repoID %q", id)

		_, _, err = LoadHashMapCache(cacheDir, id)
		require.ErrorContains(err, "not a canonical repository ID", "load repoID %q", id)
	}

	// The rejected saves must not have created anything, in or out of cacheDir.
	_, err := os.Stat(cacheDir)
	require.True(os.IsNotExist(err), "rejected saves must not create the cache dir")
}
