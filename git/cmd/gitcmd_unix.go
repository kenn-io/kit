//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package gitcmd

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func prepareGitCommand(cmd *exec.Cmd, _ bool, preserveForeground bool) {
	if preserveForeground {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

func runProcessTreeCommand(cmd *exec.Cmd) error {
	err := cmd.Run()
	if err == nil || errors.Is(err, exec.ErrWaitDelay) && rootProcessSucceeded(cmd) ||
		cmd.Process == nil || cmd.SysProcAttr == nil ||
		!cmd.SysProcAttr.Setpgid {
		return err
	}
	killErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(killErr, syscall.ESRCH) {
		killErr = nil
	}
	return errors.Join(err, killErr)
}
