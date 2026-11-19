//go:build windows

package daemon

import (
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListedProcessCreateTimeMatchesOpenedIdentity(t *testing.T) {
	cmd, _ := startLivePIDHelper(t)
	for _, pid := range []int{os.Getpid(), cmd.Process.Pid} {
		identity, ok := ReadProcessIdentity(pid)
		require.True(t, ok)
		created, err := listedProcessCreateTime(pid)
		require.NoError(t, err)
		assert.Equal(t, string(identity), strconv.FormatInt(created, 10))
	}
}

func TestListedProcessCreateTimeRejectsAnExitedProcess(t *testing.T) {
	cmd, stdin := startLivePIDHelper(t)
	pid := cmd.Process.Pid
	require.NoError(t, stdin.Close())
	require.NoError(t, cmd.Wait())
	_, err := listedProcessCreateTime(pid)
	assert.Error(t, err)
}
