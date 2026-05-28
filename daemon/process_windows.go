//go:build windows

package daemon

import (
	"errors"
	"syscall"
)

// ProcessAlive reports whether pid appears to name a live process.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const processQueryLimitedInformation = 0x1000
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	_ = syscall.CloseHandle(handle)
	return true
}
