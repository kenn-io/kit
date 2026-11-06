//go:build linux

package daemon

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestProcessAliveRejectsUnreapedChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLivePIDHelper$")
	cmd.Env = append(os.Environ(), "KIT_DAEMON_LIVE_PID_HELPER=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = stdin.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	ready, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", ready)
	require.True(t, ProcessAlive(cmd.Process.Pid))
	require.NoError(t, stdin.Close())

	var waitErr error
	require.Eventually(t, func() bool {
		var info unix.Siginfo
		waitErr = unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil)
		return waitErr != nil || info.Signo != 0
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, waitErr)
	require.NoError(t, unix.Kill(cmd.Process.Pid, 0), "signal 0 still accepts the unreaped child")
	assert.False(t, ProcessAlive(cmd.Process.Pid))
	require.NoError(t, cmd.Wait())
}
