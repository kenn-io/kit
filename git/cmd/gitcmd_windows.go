//go:build windows

package gitcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func prepareGitCommand(cmd *exec.Cmd, hideConsoleWindow, _ bool) {
	if hideConsoleWindow {
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		// Console-less callers otherwise cause git.exe to allocate a visible window.
		cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
	}
}

func runProcessTreeCommand(cmd *exec.Cmd) error {
	job, err := createKillOnCloseJob()
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(job) }()

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Starting suspended closes the assignment race: the root cannot create a
	// descendant before it and all future descendants inherit job membership.
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	cancellation := windowsJobCancellation{job: job}
	cmd.Cancel = cancellation.cancel
	if err := cmd.Start(); err != nil {
		return err
	}

	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false, uint32(cmd.Process.Pid),
	)
	if err != nil {
		return abortSuspendedProcess(cmd, fmt.Errorf("open process for job assignment: %w", err))
	}
	assignErr := windows.AssignProcessToJobObject(job, process)
	closeErr := windows.CloseHandle(process)
	if assignErr != nil {
		return abortSuspendedProcess(cmd, fmt.Errorf("assign process to Job Object: %w", assignErr))
	}
	if closeErr != nil {
		return abortJobProcess(cmd, job, fmt.Errorf("close process assignment handle: %w", closeErr))
	}
	if cancellation.markAssigned() {
		return abortJobProcess(cmd, job, context.Canceled)
	}
	if err := resumeWindowsProcess(uint32(cmd.Process.Pid)); err != nil {
		return abortJobProcess(cmd, job, err)
	}
	return cmd.Wait()
}

type windowsJobCancellation struct {
	job      windows.Handle
	mu       sync.Mutex
	assigned bool
	canceled bool
}

func (c *windowsJobCancellation) cancel() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.assigned {
		c.canceled = true
		return nil
	}
	return windows.TerminateJobObject(c.job, 1)
}

func (c *windowsJobCancellation) markAssigned() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.assigned = true
	return c.canceled
}

func createKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create process Job Object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("configure process Job Object: %w", err)
	}
	return job, nil
}

func resumeWindowsProcess(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("enumerate suspended process threads: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return fmt.Errorf("read suspended process threads: %w", err)
	}
	resumed := false
	for {
		if entry.OwnerProcessID == pid {
			thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if openErr != nil {
				return fmt.Errorf("open suspended process thread: %w", openErr)
			}
			_, resumeErr := windows.ResumeThread(thread)
			closeErr := windows.CloseHandle(thread)
			if resumeErr != nil || closeErr != nil {
				return errors.Join(windowsThreadError("resume", resumeErr), windowsThreadError("close", closeErr))
			}
			resumed = true
		}
		err = windows.Thread32Next(snapshot, &entry)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			break
		}
		if err != nil {
			return fmt.Errorf("read suspended process threads: %w", err)
		}
	}
	if !resumed {
		return fmt.Errorf("suspended process had no discoverable thread")
	}
	return nil
}

func windowsThreadError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s process thread: %w", operation, err)
}

func abortSuspendedProcess(cmd *exec.Cmd, cause error) error {
	killErr := cmd.Process.Kill()
	waitErr := cmd.Wait()
	return errors.Join(cause, ignoreProcessDone(killErr), ignoreExitError(waitErr))
}

func abortJobProcess(cmd *exec.Cmd, job windows.Handle, cause error) error {
	terminateErr := windows.TerminateJobObject(job, 1)
	waitErr := cmd.Wait()
	return errors.Join(cause, ignoreProcessDone(terminateErr), ignoreExitError(waitErr))
}

func ignoreProcessDone(err error) error {
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func ignoreExitError(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	return err
}
