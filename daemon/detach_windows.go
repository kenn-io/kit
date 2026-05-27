//go:build windows

package daemon

import "os/exec"

func detachChild(_ *exec.Cmd) {}
