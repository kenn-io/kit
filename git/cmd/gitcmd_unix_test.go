//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package gitcmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunnerCommandCancelsEntireProcessGroup(t *testing.T) {
	cmd := New().Command(context.Background(), t.TempDir(), "status")

	require.NotNil(t, cmd.SysProcAttr)
	assert.True(t, cmd.SysProcAttr.Setpgid)
	assert.NotNil(t, cmd.Cancel)
}
