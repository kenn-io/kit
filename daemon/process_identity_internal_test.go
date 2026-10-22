package daemon

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeRecordBlocksStartScope(t *testing.T) {
	pid := startLivePIDHelper(t)
	identity, ok := ReadProcessIdentity(pid)
	require.True(t, ok)
	rec := RuntimeRecord{PID: pid, ProcessIdentityV2: identity}

	assert.False(t, runtimeRecordBlocksStart(rec, ProcessScopeForeign))
	assert.True(t, runtimeRecordBlocksStart(rec, ProcessScopeLocal))
}

func startLivePIDHelper(t *testing.T) int {
	t.Helper()
	require := require.New(t)

	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), os.Args[0], "-test.run=^TestLivePIDHelper$")
	cmd.Env = append(os.Environ(), "KIT_DAEMON_LIVE_PID_HELPER=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(err)
	require.NoError(cmd.Start())
	t.Cleanup(func() {
		require.NoError(stdin.Close())
		require.NoError(cmd.Wait())
	})

	ready, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(err)
	require.Equal("ready\n", ready)
	return cmd.Process.Pid
}
