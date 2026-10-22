package daemon

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeRecordBlocksStartScope(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("PID namespaces are Linux-specific")
	}
	pid := startLivePIDHelper(t)
	identity, ok := ReadProcessIdentity(pid)
	require.True(t, ok)
	fields := strings.Split(string(identity), ":")
	require.Len(t, fields, 4)
	fields[2] = "12345"
	rec := RuntimeRecord{PID: pid, ProcessIdentityV2: ProcessIdentity(strings.Join(fields, ":"))}

	assert.False(t, runtimeRecordBlocksStart(rec, ProcessScopeForeign))
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
