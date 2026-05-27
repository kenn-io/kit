//go:build !windows

package daemon_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestRuntimeStoreRepairsPublicDir(t *testing.T) {
	dir := filepath.Join("/tmp", fmt.Sprintf("kit-runtime-public-%d", os.Getpid()))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.Chmod(dir, 0o777))

	store := daemon.RuntimeStore{Dir: dir}
	rec := daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:7474",
	}
	_, err := store.Write(rec)
	require.NoError(t, err)
	_, err = store.List()
	require.NoError(t, err)
	_, err = store.CleanupDead()
	require.NoError(t, err)
	_, err = store.LockPath()
	require.NoError(t, err)
	_, err = store.Read(filepath.Join(dir, "daemon.1.json"))
	require.Error(t, err)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Zero(t, info.Mode().Perm()&0o077)
}

func TestRuntimeStoreRepairsPrivateUnusableDir(t *testing.T) {
	dir := filepath.Join("/tmp", fmt.Sprintf("kit-runtime-unusable-%d", os.Getpid()))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.Chmod(dir, 0o500))

	store := daemon.RuntimeStore{Dir: dir}
	_, err := store.LockPath()
	require.NoError(t, err)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestRuntimeStoreRejectsSymlinkDir(t *testing.T) {
	base := filepath.Join("/tmp", fmt.Sprintf("kit-runtime-symlink-%d", os.Getpid()))
	target := base + "-target"
	t.Cleanup(func() {
		_ = os.Remove(base)
		_ = os.RemoveAll(target)
	})
	require.NoError(t, os.RemoveAll(base))
	require.NoError(t, os.RemoveAll(target))
	require.NoError(t, os.MkdirAll(target, 0o700))
	require.NoError(t, os.Symlink(target, base))

	store := daemon.RuntimeStore{Dir: base}
	_, err := store.LockPath()
	require.Error(t, err)
	_, err = store.Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:7474",
	})
	require.Error(t, err)
	_, err = store.List()
	require.Error(t, err)
	_, err = store.CleanupDead()
	require.Error(t, err)
}

func TestRuntimeStoreRejectsSymlinkRecord(t *testing.T) {
	dir := filepath.Join("/tmp", fmt.Sprintf("kit-runtime-record-symlink-%d", os.Getpid()))
	target := filepath.Join(dir, "target.json")
	link := filepath.Join(dir, "daemon.1.json")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(target, []byte(`{"pid":1,"address":"127.0.0.1:7474"}`), 0o644))
	require.NoError(t, os.Symlink(target, link))

	store := daemon.RuntimeStore{Dir: dir}
	_, err := store.Read(link)
	require.Error(t, err)
}
