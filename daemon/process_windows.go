//go:build windows

package daemon

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

var openProcessForLiveness = syscall.OpenProcess

// ProcessAlive reports whether pid appears to name a live process.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const processQueryLimitedInformation = 0x1000
	handle, err := openProcessForLiveness(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		// Only an invalid PID proves absence when the process cannot be opened.
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer func() { _ = syscall.CloseHandle(handle) }()

	// A terminated process object remains openable while another handle keeps
	// it alive. The exit status, not the handle alone, distinguishes that state.
	const stillActive = 259
	var exitCode uint32
	if err := syscall.GetExitCodeProcess(handle, &exitCode); err != nil {
		return true
	}
	return exitCode == stillActive
}
