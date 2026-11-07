package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// startLivePIDHelper starts a child process that lives until test cleanup.
func startLivePIDHelper(t *testing.T) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	require := require.New(t)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLivePIDHelper$")
	cmd.Env = append(os.Environ(), "KIT_DAEMON_LIVE_PID_HELPER=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(err)
	require.NoError(cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = stdin.Close()
			require.NoError(cmd.Wait())
		}
	})

	ready, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(err)
	require.Equal("ready\n", ready)
	return cmd, stdin
}

func TestLivePIDHelper(t *testing.T) {
	if os.Getenv("KIT_DAEMON_LIVE_PID_HELPER") != "1" {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}
