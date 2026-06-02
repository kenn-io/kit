package daemon_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestRuntimeStoreWriteListAndRead(t *testing.T) {
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	ep := daemon.Endpoint{Network: daemon.NetworkTCP, Address: "127.0.0.1:7474"}
	rec := daemon.NewRuntimeRecord("kata", "v1", ep)
	rec.StartedAt = time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	rec.Metadata = map[string]string{"db_path": "/tmp/kata.db"}

	path, err := store.Write(rec)
	require.NoError(t, err)
	assert.Equal(t, "daemon."+strconv.Itoa(os.Getpid())+".json", filepath.Base(path))

	records, err := store.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	got := records[0]
	assert.Equal(t, os.Getpid(), got.PID)
	assert.Equal(t, "kata", got.Service)
	assert.Equal(t, "v1", got.Version)
	assert.Equal(t, ep, got.Endpoint())
	assert.Equal(t, path, got.SourcePath)
	assert.Equal(t, "/tmp/kata.db", got.Metadata["db_path"])
}

func TestRuntimeStoreCleanupDeadLeavesMismatchedFiles(t *testing.T) {
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	deadPID := 999999
	if daemon.ProcessAlive(deadPID) {
		t.Skipf("pid %d is alive on this host", deadPID)
	}
	dead := daemon.RuntimeRecord{
		PID:       deadPID,
		Network:   daemon.NetworkTCP,
		Address:   "127.0.0.1:7474",
		StartedAt: time.Now(),
	}
	_, err := store.Write(dead)
	require.NoError(t, err)

	mismatchPath, err := store.Path(deadPID + 1)
	require.NoError(t, err)
	err = os.WriteFile(mismatchPath, []byte(`{"pid":999999,"address":"127.0.0.1:7475"}`), 0o644)
	require.NoError(t, err)

	removed, err := store.CleanupDead()
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	deadPath, err := store.Path(deadPID)
	require.NoError(t, err)
	_, err = os.Stat(deadPath)
	assert.True(t, os.IsNotExist(err), "dead runtime still exists or unexpected stat error: %v", err)
	_, err = os.Stat(mismatchPath)
	require.NoError(t, err)
}

func TestRuntimeStoreRejectsPrefixTraversal(t *testing.T) {
	store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: "../escape"}

	_, err := store.Path(123)
	require.Error(t, err)
	_, err = store.Write(daemon.RuntimeRecord{
		PID:     123,
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:7474",
	})
	require.Error(t, err)
	_, err = store.List()
	require.Error(t, err)
	_, err = store.CleanupDead()
	require.Error(t, err)
}

func TestRuntimeStoreListenLockPathIsSeparateFromStartLock(t *testing.T) {
	store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: "kata"}

	startLock, err := store.LockPath()
	require.NoError(t, err)
	listenLock, err := store.ListenLockPath()
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(store.Dir, "kata.lock"), startLock)
	assert.Equal(t, filepath.Join(store.Dir, "kata.listen.lock"), listenLock)
}
