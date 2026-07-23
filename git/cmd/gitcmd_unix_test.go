//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package gitcmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

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

func TestFailedCommandTerminatesBackgroundDescendants(t *testing.T) {
	const helperMode = "KIT_GITCMD_UNIX_FAILURE_HELPER"
	mode := os.Getenv(helperMode)
	marker := os.Getenv("KIT_GITCMD_UNIX_FAILURE_MARKER")
	switch mode {
	case "child":
		time.Sleep(500 * time.Millisecond)
		_ = os.WriteFile(marker, []byte("escaped"), 0o600)
		return
	case "parent":
		child := exec.Command(
			os.Args[0], "-test.run=^TestFailedCommandTerminatesBackgroundDescendants$",
		)
		child.Env = append(os.Environ(), helperMode+"=child")
		_ = child.Start()
		os.Exit(3)
	}

	marker = filepath.Join(t.TempDir(), "descendant-finished")
	parent := exec.CommandContext(
		t.Context(), os.Args[0],
		"-test.run=^TestFailedCommandTerminatesBackgroundDescendants$",
	)
	parent.Env = append(os.Environ(), helperMode+"=parent",
		"KIT_GITCMD_UNIX_FAILURE_MARKER="+marker)
	PrepareProcessTreeCancellation(parent, false)

	err := RunProcessTreeCommand(parent)

	require.Error(t, err)
	time.Sleep(time.Second)
	assert.NoFileExists(t, marker)
}
