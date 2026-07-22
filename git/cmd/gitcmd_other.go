//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris && !windows

package gitcmd

import "os/exec"

func prepareGitCommand(*exec.Cmd, bool, bool) {}

func runProcessTreeCommand(cmd *exec.Cmd) error {
	return cmd.Run()
}
