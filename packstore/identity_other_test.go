//go:build !windows && !unix

package packstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenNoFollowRejectsSymlinkOnFallbackPlatforms(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(target, []byte("target content"), 0o600))
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable: " + err.Error())
	}

	f, err := openNoFollow(link, false)
	if f != nil {
		require.NoError(t, f.Close())
	}
	require.Error(t, err)
}
