//go:build windows

package gitcmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsStillActive = 259

func prepareGitCommand(cmd *exec.Cmd, hideConsoleWindow bool) {
	if hideConsoleWindow {
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		// Console-less callers otherwise cause git.exe to allocate a visible window.
		cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := terminateWindowsProcessDescendants(uint32(cmd.Process.Pid)); err == nil {
			return cmd.Process.Kill()
		}
		// Retain taskkill as a fallback if Windows denied process snapshot or
		// termination access. MSYS descendants are not always removed by
		// taskkill alone, which is why the explicit tree walk runs first.
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		kill.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		if err := kill.Run(); err == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}

func terminateWindowsProcessDescendants(rootPID uint32) error {
	descendants, err := windowsProcessDescendants(rootPID)
	if err != nil {
		return err
	}
	var errs []error
	for _, pid := range descendants {
		process, openErr := windows.OpenProcess(
			windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
			false, pid,
		)
		if openErr != nil {
			if !errors.Is(openErr, windows.ERROR_INVALID_PARAMETER) {
				errs = append(errs, fmt.Errorf("open descendant process %d: %w", pid, openErr))
			}
			continue
		}
		terminateErr := windows.TerminateProcess(process, 1)
		if terminateErr != nil {
			var exitCode uint32
			processExited := errors.Is(terminateErr, windows.ERROR_ACCESS_DENIED) &&
				windows.GetExitCodeProcess(process, &exitCode) == nil &&
				exitCode != windowsStillActive
			if !processExited {
				errs = append(errs, fmt.Errorf("terminate descendant process %d: %w", pid, terminateErr))
			}
		}
		closeErr := windows.CloseHandle(process)
		if closeErr != nil {
			errs = append(errs, fmt.Errorf("close descendant process %d: %w", pid, closeErr))
		}
	}
	return errors.Join(errs...)
}

func windowsProcessDescendants(rootPID uint32) ([]uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()

	children := make(map[uint32][]uint32)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, err
	}
	for {
		children[entry.ParentProcessID] = append(children[entry.ParentProcessID], entry.ProcessID)
		err = windows.Process32Next(snapshot, &entry)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			break
		}
		if err != nil {
			return nil, err
		}
	}

	result := make([]uint32, 0)
	visited := map[uint32]bool{rootPID: true}
	var appendChildren func(uint32)
	appendChildren = func(parent uint32) {
		for _, child := range children[parent] {
			if child == 0 || visited[child] {
				continue
			}
			visited[child] = true
			appendChildren(child)
			result = append(result, child)
		}
	}
	appendChildren(rootPID)
	return result, nil
}
