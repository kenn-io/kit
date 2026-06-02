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
	assert := assert.New(t)
	require := require.New(t)

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	ep := daemon.Endpoint{Network: daemon.NetworkTCP, Address: "127.0.0.1:7474"}
	rec := daemon.NewRuntimeRecord("kata", "v1", ep)
	rec.StartedAt = time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	rec.Metadata = map[string]string{"db_path": "/tmp/kata.db"}

	path, err := store.Write(rec)
	require.NoError(err)
	assert.Equal("daemon."+strconv.Itoa(os.Getpid())+".json", filepath.Base(path))

	records, err := store.List()
	require.NoError(err)
	require.Len(records, 1)
	got := records[0]
	assert.Equal(os.Getpid(), got.PID)
	assert.Equal("kata", got.Service)
	assert.Equal("v1", got.Version)
	assert.Equal(ep, got.Endpoint())
	assert.Equal(path, got.SourcePath)
	assert.Equal("/tmp/kata.db", got.Metadata["db_path"])
}

func TestRuntimeStoreCleanupDeadLeavesMismatchedFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)

	mismatchPath, err := store.Path(deadPID + 1)
	require.NoError(err)
	err = os.WriteFile(mismatchPath, []byte(`{"pid":999999,"address":"127.0.0.1:7475"}`), 0o644)
	require.NoError(err)

	removed, err := store.CleanupDead()
	require.NoError(err)
	assert.Equal(1, removed)
	deadPath, err := store.Path(deadPID)
	require.NoError(err)
	_, err = os.Stat(deadPath)
	assert.True(os.IsNotExist(err), "dead runtime still exists or unexpected stat error: %v", err)
	_, err = os.Stat(mismatchPath)
	require.NoError(err)
}

func TestRuntimeStoreRejectsPrefixTraversal(t *testing.T) {
	require := require.New(t)

	store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: "../escape"}

	_, err := store.Path(123)
	require.Error(err)
	_, err = store.Write(daemon.RuntimeRecord{
		PID:     123,
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:7474",
	})
	require.Error(err)
	_, err = store.List()
	require.Error(err)
	_, err = store.CleanupDead()
	require.Error(err)
}

func TestRuntimeStoreRejectsRelativeDirBeforePreparing(t *testing.T) {
	assert := assert.New(t)

	store := daemon.RuntimeStore{Dir: "relative-runtime"}

	_, err := store.LockPath()
	require.Error(t, err)
	assert.Contains(err.Error(), "must be absolute")

	_, statErr := os.Stat("relative-runtime")
	assert.True(os.IsNotExist(statErr), "relative runtime dir should not be created: %v", statErr)
}

func TestRuntimeStoreListenLockPathIsSeparateFromStartLock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: "kata"}

	startLock, err := store.LockPath()
	require.NoError(err)
	listenLock, err := store.ListenLockPath()
	require.NoError(err)

	assert.Equal(filepath.Join(store.Dir, "kata.lock"), startLock)
	assert.Equal(filepath.Join(store.Dir, "kata.listen.lock"), listenLock)
}
