//go:build windows

package gitcmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestRunnerCommandHidesGitConsoleWindow(t *testing.T) {
	cmd := New().Command(context.Background(), "", "status")

	require.NotNil(t, cmd.SysProcAttr)
	assert.NotZero(t, cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW, "git subprocesses must not flash console windows")
}

func TestRunnerCommandAllowsConsoleWindowForTerminalPrompts(t *testing.T) {
	runner := New()
	runner.TerminalPrompt = true

	cmd := runner.Command(context.Background(), "", "fetch")

	if cmd.SysProcAttr != nil {
		assert.Zero(t, cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW, "interactive git prompts should be able to use the console")
	}
}

func TestProcessTreeCancellationStopsLateDescendants(t *testing.T) {
	const helperMode = "KIT_GITCMD_WINDOWS_TREE_HELPER"
	mode := os.Getenv(helperMode)
	marker := os.Getenv("KIT_GITCMD_WINDOWS_TREE_MARKER")
	ready := os.Getenv("KIT_GITCMD_WINDOWS_TREE_READY")
	if mode == "child" {
		time.Sleep(1500 * time.Millisecond)
		_ = os.WriteFile(marker, []byte("escaped"), 0o600)
		return
	}
	if mode == "parent" {
		_ = os.WriteFile(ready, []byte("ready"), 0o600)
		deadline := time.Now().Add(750 * time.Millisecond)
		for time.Now().Before(deadline) {
			child := exec.Command(os.Args[0], "-test.run=^TestProcessTreeCancellationStopsLateDescendants$")
			child.Env = append(os.Environ(), helperMode+"=child")
			_ = child.Start()
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(5 * time.Second)
		return
	}

	require := require.New(t)
	assert := assert.New(t)
	marker = filepath.Join(t.TempDir(), "escaped")
	ready = marker + ".ready"
	ctx, cancel := context.WithCancel(t.Context())
	parent := exec.CommandContext(
		ctx, os.Args[0], "-test.run=^TestProcessTreeCancellationStopsLateDescendants$",
	)
	parent.Env = append(os.Environ(),
		helperMode+"=parent", "KIT_GITCMD_WINDOWS_TREE_MARKER="+marker,
		"KIT_GITCMD_WINDOWS_TREE_READY="+ready,
	)
	PrepareProcessTreeCancellation(parent, true)
	require.NoError(parent.Start())
	require.Eventually(func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	cancel()
	_ = parent.Wait()
	time.Sleep(2 * time.Second)
	assert.NoFileExists(marker)
}
