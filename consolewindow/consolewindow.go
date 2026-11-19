// Package consolewindow controls console windows for child processes.
package consolewindow

import (
	"context"
	"os/exec"
)

// Hide prevents a non-interactive child from opening a console window on Windows.
// Call it before starting cmd when its output is captured or discarded.
// It preserves existing process flags and does nothing on other platforms.
// Windows ignores this flag with DETACHED_PROCESS or CREATE_NEW_CONSOLE;
// use daemon.StartDetached for detached daemons.
func Hide(cmd *exec.Cmd) {
	hide(cmd)
}

// CommandContext is exec.CommandContext for a non-interactive child, with its
// console window hidden on Windows. A child that must use the caller's
// console, such as one that prompts, uses exec.CommandContext directly.
func CommandContext(ctx context.Context, name string, arg ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, arg...) //nolint:forbidigo // the hidden constructor the lint rule points to
	hide(cmd)
	return cmd
}
