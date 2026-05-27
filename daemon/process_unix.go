//go:build !windows

package daemon

import (
	"os"
	"syscall"
)

// ProcessAlive reports whether pid appears to name a live process.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}
