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

type gitWindowsProcess struct {
	pid     uint32
	created int64
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
	children, err := snapshotGitProcesses()
	if err != nil {
		return err
	}
	return killGitChildren(pid, int64(uint64(created.HighDateTime)<<32|uint64(created.LowDateTime)), children)
}

func snapshotGitProcesses() (map[uint32][]gitWindowsProcess, error) {
	// Unlike Toolhelp, this snapshot includes creation times. Retain those
	// identities even when an intermediate process exits before cancellation.
	buffer := make([]byte, 64<<10)
	for {
		var needed uint32
		err := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&buffer[0]), uint32(len(buffer)), &needed)
		if errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) {
			buffer = make([]byte, int(needed)+(64<<10))
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("snapshot Git subprocesses: %w", err)
		}
		break
	}
	children := make(map[uint32][]gitWindowsProcess)
	for offset := uint32(0); ; {
		entry := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&buffer[offset]))
		parent := uint32(entry.InheritedFromUniqueProcessID)
		children[parent] = append(children[parent], gitWindowsProcess{pid: uint32(entry.UniqueProcessID), created: entry.CreateTime})
		if entry.NextEntryOffset == 0 {
			return children, nil
		}
		offset += entry.NextEntryOffset
	}
}

func killGitChildren(parent uint32, parentCreated int64, children map[uint32][]gitWindowsProcess) error {
	descendants := children[parent]
	delete(children, parent)
	var result error
	for _, child := range descendants {
		// Parent IDs can outlive their processes and then be reused by Git.
		if child.created < parentCreated {
			continue
		}
		// Traverse first: an exited intermediate helper can still have live
		// descendants, whose captured identities remain valid.
		result = errors.Join(result, killGitChildren(child.pid, child.created, children), killGitChild(child))
	}
	return result
}

func killGitChild(child gitWindowsProcess) error {
	process, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, child.pid)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil // The child already exited.
		}
		return fmt.Errorf("open Git subprocess %d: %w", child.pid, err)
	}
	defer func() { _ = windows.CloseHandle(process) }()
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &created, &exited, &kernel, &user); err != nil {
		return fmt.Errorf("read Git subprocess %d creation time: %w", child.pid, err)
	}
	// Keep the handle open through termination, and reject a reused PID.
	if int64(uint64(created.HighDateTime)<<32|uint64(created.LowDateTime)) != child.created {
		return nil
	}
	if err := windows.TerminateProcess(process, 1); err != nil {
		state, waitErr := windows.WaitForSingleObject(process, 0)
		if waitErr != nil || state != windows.WAIT_OBJECT_0 {
			return errors.Join(fmt.Errorf("stop Git subprocess %d: %w", child.pid, err), waitErr)
		}
	}
	return nil
}
