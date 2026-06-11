//go:build windows

package gitcmd

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func prepareGitCommand(cmd *exec.Cmd, hideConsoleWindow bool) {
	if !hideConsoleWindow {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Console-less callers otherwise cause git.exe to allocate a visible window.
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
