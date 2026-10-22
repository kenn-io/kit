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
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), os.Args[0], "-test.run=^TestLivePIDHelper$")
	cmd.Env = append(os.Environ(), "KIT_DAEMON_LIVE_PID_HELPER=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		assert.NoError(t, stdin.Close())
		assert.NoError(t, cmd.Wait())
	})

	ready, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", ready)
	identity, ok := ReadProcessIdentity(cmd.Process.Pid)
	require.True(t, ok)
	rec := RuntimeRecord{PID: cmd.Process.Pid, ProcessIdentityV2: identity}

	assert.False(t, runtimeRecordBlocksStart(rec, ProcessScopeForeign))
	assert.True(t, runtimeRecordBlocksStart(rec, ProcessScopeLocal))
}
