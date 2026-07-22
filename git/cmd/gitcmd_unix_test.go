//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package gitcmd

import (
	"context"
	"os/exec"
	"syscall"
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

func TestProcessTreeSetupPreservesCallerSysProcAttr(t *testing.T) {
	existing := &syscall.SysProcAttr{}
	cmd := exec.CommandContext(t.Context(), "git", "status")
	cmd.SysProcAttr = existing

	prepareGitCommand(cmd, false, false)

	assert.Same(t, existing, cmd.SysProcAttr)
	assert.True(t, cmd.SysProcAttr.Setpgid)
}

func TestSafeDirectoryProbeUsesNonInteractiveProcessSettings(t *testing.T) {
	cmd := safeDirectoryProbeCommand(context.Background(), nil, "")

	require.NotNil(t, cmd.SysProcAttr)
	assert.True(t, cmd.SysProcAttr.Setpgid)
	assert.NotNil(t, cmd.Cancel)
}
