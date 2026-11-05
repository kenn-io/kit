//go:build windows

package gitcmd

import (
	"errors"
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"

	"go.kenn.io/kit/consolewindow"
)

func prepareGitCommand(cmd *exec.Cmd, hideConsoleWindow bool) {
	if hideConsoleWindow {
		consolewindow.Hide(cmd)
	}
	cmd.Cancel = func() error {
		// Stop descendants before Git. Starting taskkill here can exhaust its
		// cancellation budget on a busy host and leave HTTP helpers running.
		err := killGitDescendants(uint32(cmd.Process.Pid))
		return errors.Join(err, cmd.Process.Kill())
	}
}

func killGitDescendants(pid uint32) error {
	root, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return fmt.Errorf("open Git process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(root) }()
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(root, &created, &exited, &kernel, &user); err != nil {
		return fmt.Errorf("read Git process creation time: %w", err)
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return fmt.Errorf("snapshot Git subprocesses: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	var snapshotTime windows.Filetime
	windows.GetSystemTimeAsFileTime(&snapshotTime)

	children := make(map[uint32][]uint32)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		children[entry.ParentProcessID] = append(children[entry.ParentProcessID], entry.ProcessID)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("enumerate Git subprocesses: %w", err)
	}

	var killChildren func(uint32, int64) error
	killChildren = func(parent uint32, parentCreated int64) error {
		var result error
		for _, child := range children[parent] {
			delete(children, parent)
			result = errors.Join(result, killGitChild(child, parentCreated, snapshotTime.Nanoseconds(), killChildren))
		}
		return result
	}
	return killChildren(pid, created.Nanoseconds())
}

func killGitChild(pid uint32, parentCreated, snapshotTime int64, killChildren func(uint32, int64) error) error {
	process, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil // The child already exited.
		}
		return fmt.Errorf("open Git subprocess %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(process) }()
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &created, &exited, &kernel, &user); err != nil {
		return fmt.Errorf("read Git subprocess %d creation time: %w", pid, err)
	}
	// Parent IDs can outlive their processes. Ignore stale relationships and
	// IDs reused after the snapshot, and keep this handle open through the kill.
	if created.Nanoseconds() < parentCreated || created.Nanoseconds() > snapshotTime {
		return nil
	}
	result := killChildren(pid, created.Nanoseconds())
	if err := windows.TerminateProcess(process, 1); err != nil {
		state, waitErr := windows.WaitForSingleObject(process, 0)
		if waitErr != nil || state != windows.WAIT_OBJECT_0 {
			return errors.Join(result, fmt.Errorf("stop Git subprocess %d: %w", pid, err), waitErr)
		}
	}
	return result
}
