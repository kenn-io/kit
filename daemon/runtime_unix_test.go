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
