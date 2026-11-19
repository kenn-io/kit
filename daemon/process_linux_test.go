//go:build linux

package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestProcessAliveRejectsUnreapedChild(t *testing.T) {
	cmd, stdin := startLivePIDHelper(t)
	require.True(t, ProcessAlive(cmd.Process.Pid))
	rec := RuntimeRecord{PID: cmd.Process.Pid, Address: "127.0.0.1:7474"}
	store := RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(rec)
	require.NoError(t, err)
	require.NoError(t, stdin.Close())

	// WNOWAIT observes the exit but leaves the child unreaped.
	var waitErr error
	require.Eventually(t, func() bool {
		var info unix.Siginfo
		waitErr = unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil)
		return waitErr != nil || info.Signo != 0
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, waitErr)
	assert.False(t, ProcessAlive(cmd.Process.Pid))
	assert.False(t, RuntimeRecordBlocksStart(rec))
	removed, err := store.CleanupDead()
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	require.NoError(t, cmd.Wait())
}
