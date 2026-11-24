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
	alive, err := queryProcessAlive(pid)
	return alive || errors.Is(err, syscall.ERROR_ACCESS_DENIED)
}

func processConfirmedDead(pid int) bool {
	alive, err := queryProcessAlive(pid)
	return !alive && (err == nil || errors.Is(err, windows.ERROR_INVALID_PARAMETER))
}

func queryProcessAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	const processQueryLimitedInformation = 0x1000
	handle, err := openProcessForLiveness(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false, err
	}
	defer func() { _ = syscall.CloseHandle(handle) }()

	// A terminated process object remains openable while another handle keeps
	// it alive. The exit status, not the handle alone, distinguishes that state.
	const stillActive = 259
	var exitCode uint32
	if err := syscall.GetExitCodeProcess(handle, &exitCode); err != nil {
		return true, nil
	}
	return exitCode == stillActive, nil
}
