//go:build windows

package gitcmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsStillActive = 259

func prepareGitCommand(cmd *exec.Cmd, hideConsoleWindow, _ bool) {
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
		rootPID := uint32(cmd.Process.Pid)
		rootCreation, identityErr := windowsProcessCreation(rootPID)
		rootErr := cmd.Process.Kill()
		if identityErr != nil || rootErr != nil && !errors.Is(rootErr, os.ErrProcessDone) {
			return errors.Join(identityErr, rootErr)
		}
		var cutoff windows.Filetime
		windows.GetSystemTimeAsFileTime(&cutoff)
		treeErr := terminateWindowsProcessDescendants(
			rootPID, filetimeValue(rootCreation), filetimeValue(cutoff),
		)
		if errors.Is(rootErr, os.ErrProcessDone) && identityErr == nil && treeErr == nil {
			return os.ErrProcessDone
		}
		return errors.Join(identityErr, rootErr, treeErr)
	}
}

type windowsProcessIdentity struct {
	pid     uint32
	created uint64
}

func terminateWindowsProcessDescendants(
	rootPID uint32, rootCreated, rootStopped uint64,
) error {
	var errs []error
	for range 16 {
		descendants, err := windowsProcessDescendants(rootPID, rootCreated, rootStopped)
		if err != nil {
			errs = append(errs, err)
		}
		if len(descendants) == 0 {
			return errors.Join(errs...)
		}
		for _, process := range descendants {
			if err := terminateWindowsProcess(process); err != nil {
				errs = append(errs, err)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.Join(append(errs,
		fmt.Errorf("descendant processes remained after repeated cancellation"))...)
}

func terminateWindowsProcess(identity windowsProcessIdentity) error {
	process, openErr := windows.OpenProcess(
		windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, identity.pid,
	)
	if openErr != nil {
		if errors.Is(openErr, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return fmt.Errorf("open descendant process %d: %w", identity.pid, openErr)
	}
	defer func() { _ = windows.CloseHandle(process) }()
	created, err := windowsProcessCreationFromHandle(process)
	if err != nil {
		return fmt.Errorf("identify descendant process %d: %w", identity.pid, err)
	}
	if filetimeValue(created) != identity.created {
		return fmt.Errorf("descendant process %d identity changed before termination", identity.pid)
	}
	terminateErr := windows.TerminateProcess(process, 1)
	if terminateErr != nil {
		var exitCode uint32
		processExited := errors.Is(terminateErr, windows.ERROR_ACCESS_DENIED) &&
			windows.GetExitCodeProcess(process, &exitCode) == nil &&
			exitCode != windowsStillActive
		if !processExited {
			return fmt.Errorf("terminate descendant process %d: %w", identity.pid, terminateErr)
		}
	}
	return nil
}

func windowsProcessCreation(pid uint32) (windows.Filetime, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return windows.Filetime{}, err
	}
	defer func() { _ = windows.CloseHandle(process) }()
	return windowsProcessCreationFromHandle(process)
}

func windowsProcessCreationFromHandle(process windows.Handle) (windows.Filetime, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &created, &exited, &kernel, &user); err != nil {
		return windows.Filetime{}, err
	}
	return created, nil
}

func filetimeValue(value windows.Filetime) uint64 {
	return uint64(value.HighDateTime)<<32 | uint64(value.LowDateTime)
}

func windowsProcessDescendants(
	rootPID uint32, rootCreated, rootStopped uint64,
) ([]windowsProcessIdentity, error) {
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

	result := make([]windowsProcessIdentity, 0)
	var identityErrs []error
	visited := map[uint32]bool{rootPID: true}
	var appendChildren func(uint32, uint64, bool)
	appendChildren = func(parent uint32, parentCreated uint64, direct bool) {
		for _, pid := range children[parent] {
			if pid == 0 || visited[pid] {
				continue
			}
			visited[pid] = true
			created, creationErr := windowsProcessCreation(pid)
			if creationErr != nil {
				if !errors.Is(creationErr, windows.ERROR_INVALID_PARAMETER) {
					identityErrs = append(identityErrs,
						fmt.Errorf("identify descendant process %d: %w", pid, creationErr))
				}
				continue
			}
			createdValue := filetimeValue(created)
			if createdValue < parentCreated || direct && createdValue > rootStopped {
				continue
			}
			appendChildren(pid, createdValue, false)
			result = append(result, windowsProcessIdentity{
				pid: pid, created: createdValue,
			})
		}
	}
	appendChildren(rootPID, rootCreated, true)
	return result, errors.Join(identityErrs...)
}
