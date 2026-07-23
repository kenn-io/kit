//go:build windows

package gitcmd

import (
	"context"
	"testing"

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
