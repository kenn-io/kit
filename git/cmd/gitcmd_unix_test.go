//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package gitcmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInteractiveCommandPreservesForegroundProcessGroup(t *testing.T) {
	interactive := New()
	interactive.TerminalPrompt = true
	interactiveCommand := interactive.Command(context.Background(), "", "status")

	nonInteractiveCommand := New().Command(context.Background(), "", "status")

	assert.Nil(t, interactiveCommand.SysProcAttr)
	assert.NotNil(t, interactiveCommand.Cancel)
	require.NotNil(t, nonInteractiveCommand.SysProcAttr)
	assert.True(t, nonInteractiveCommand.SysProcAttr.Setpgid)
	assert.NotNil(t, nonInteractiveCommand.Cancel)
}
