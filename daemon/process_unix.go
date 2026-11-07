//go:build !windows

package daemon

import (
	"errors"
	"os"
	"syscall"
)

// ProcessAlive reports whether pid appears to name a live process.
// On Linux, a fully exited process awaiting reaping counts as gone.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return (err == nil || errors.Is(err, syscall.EPERM)) && !processHasExited(pid)
}
