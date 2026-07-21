//go:build unix && !linux

package packstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHardlinkIdentityPinCloseCleansOwnedPathWithoutCapturedIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "exclusive-pin")
	require.NoError(t, os.Mkdir(dir, 0o700))
	path := filepath.Join(dir, "pinned")
	require.NoError(t, os.WriteFile(path, []byte("owned pin"), 0o600))
	pin := &hardlinkIdentityPin{path: path, dir: dir}

	var closeErr error
	require.NotPanics(t, func() {
		closeErr = pin.Close()
	})
	require.NoError(t, closeErr)
	assert.NoFileExists(t, path)
	assert.NoDirExists(t, dir)
}

func TestHardlinkIdentityPinStatRejectsPrivatePathReplacement(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "exclusive-pin")
	require.NoError(t, os.Mkdir(dir, 0o700))
	path := filepath.Join(dir, "pinned")
	require.NoError(t, os.WriteFile(path, []byte("verified pin"), 0o600))
	identity, err := os.Lstat(path)
	require.NoError(t, err)
	require.NoError(t, os.Link(path, filepath.Join(dir, "held")), "keep the verified inode allocated")
	pin := &hardlinkIdentityPin{path: path, dir: dir, identity: identity}
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, []byte("replacement"), 0o600))

	_, err = pin.Stat()

	require.ErrorIs(t, err, errIdentityChanged)
}
