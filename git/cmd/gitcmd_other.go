//go:build !windows

package gitcmd

import "os/exec"

func prepareGitCommand(*exec.Cmd, bool) {}
