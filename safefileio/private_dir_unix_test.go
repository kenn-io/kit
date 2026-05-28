//go:build !windows

package safefileio_test

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func TestEnsurePrivateDirRepairsPublicDir(t *testing.T) {
	dir := filepath.Join("/tmp", fmt.Sprintf("kit-safefileio-public-%d", os.Getpid()))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.Chmod(dir, 0o777))

	require.NoError(t, safefileio.EnsurePrivateDir(dir))

	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestEnsurePrivateDirRejectsEmptyPath(t *testing.T) {
	require.Error(t, safefileio.EnsurePrivateDir(""))
}

func TestEnsurePrivateDirRejectsSymlink(t *testing.T) {
	base := filepath.Join("/tmp", fmt.Sprintf("kit-safefileio-symlink-%d", os.Getpid()))
	target := base + "-target"
	t.Cleanup(func() {
		_ = os.Remove(base)
		_ = os.RemoveAll(target)
	})
	require.NoError(t, os.RemoveAll(base))
	require.NoError(t, os.RemoveAll(target))
	require.NoError(t, os.MkdirAll(target, 0o700))
	require.NoError(t, os.Symlink(target, base))

	require.Error(t, safefileio.EnsurePrivateDir(base))
}

func TestOpenCurrentUserFileRejectsEmptyPath(t *testing.T) {
	file, err := safefileio.OpenCurrentUserFile("")
	require.Error(t, err)
	require.Nil(t, file)
}

func TestOpenCurrentUserFileRejectsNonRegularFile(t *testing.T) {
	dir := filepath.Join("/tmp", fmt.Sprintf("kit-safefileio-fifo-%d", os.Getpid()))
	path := filepath.Join(dir, "record.json")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, syscall.Mkfifo(path, 0o600))

	file, err := safefileio.OpenCurrentUserFile(path)
	require.Error(t, err)
	require.Nil(t, file)
}
