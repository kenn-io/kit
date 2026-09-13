//go:build windows

package gitcmd

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

func prepareGitCommand(cmd *exec.Cmd, hideConsoleWindow bool) {
	if hideConsoleWindow {
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		// Console-less callers otherwise cause git.exe to allocate a visible window.
		cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
	}
	cmd.Cancel = func() error {
		systemDir, err := windows.GetSystemDirectory()
		if err == nil {
			killCtx, cancel := context.WithTimeout(context.Background(), gitCommandWaitDelay)
			defer cancel()
			kill := exec.CommandContext(killCtx, filepath.Join(systemDir, "taskkill.exe"),
				"/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
			kill.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
			if err = kill.Run(); err == nil {
				return nil
			}
		}
		return cmd.Process.Kill()
	}
}
