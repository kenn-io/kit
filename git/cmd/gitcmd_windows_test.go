//go:build windows

package gitcmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestRunnerCommandHidesGitConsoleWindow(t *testing.T) {
	cmd := New().Command(context.Background(), "", "status")

	require.NotNil(t, cmd.SysProcAttr)
	assert.NotZero(t, cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW, "git subprocesses must not flash console windows")
}

func TestRunnerCommandAllowsConsoleWindowForTerminalPrompts(t *testing.T) {
	runner := New()
	runner.TerminalPrompt = true

	cmd := runner.Command(context.Background(), "", "fetch")

	if cmd.SysProcAttr != nil {
		assert.Zero(t, cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW, "interactive git prompts should be able to use the console")
	}
}

func TestWindowsJobCancellationLatchesBeforeAssignment(t *testing.T) {
	cancellation := windowsJobCancellation{}

	require.NoError(t, cancellation.cancel())
	assert.True(t, cancellation.markAssigned())
}

func TestProcessTreeJobKillsMembersWhenClosed(t *testing.T) {
	job, err := createKillOnCloseJob()
	require.NoError(t, err)
	defer windows.CloseHandle(job)

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	err = windows.QueryInformationJobObject(
		job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil,
	)
	require.NoError(t, err)
	assert.NotZero(t,
		info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE)
}

func TestProcessTreeJobCanPreserveMembersAfterSuccessfulCommand(t *testing.T) {
	job, err := createKillOnCloseJob()
	require.NoError(t, err)
	defer windows.CloseHandle(job)

	require.NoError(t, disableJobKillOnClose(job))

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	err = windows.QueryInformationJobObject(
		job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil,
	)
	require.NoError(t, err)
	assert.Zero(t,
		info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE)
}

func TestSuccessfulProcessPreservesDetachedDescendant(t *testing.T) {
	const helperMode = "KIT_GITCMD_WINDOWS_SUCCESS_HELPER"
	mode := os.Getenv(helperMode)
	marker := os.Getenv("KIT_GITCMD_WINDOWS_SUCCESS_MARKER")
	if mode == "child" {
		time.Sleep(500 * time.Millisecond)
		_ = os.WriteFile(marker, []byte("survived"), 0o600)
		return
	}
	if mode == "parent" {
		child := exec.Command(os.Args[0], "-test.run=^TestSuccessfulProcessPreservesDetachedDescendant$")
		child.Env = append(os.Environ(), helperMode+"=child")
		_ = child.Start()
		return
	}

	marker = filepath.Join(t.TempDir(), "descendant-finished")
	parent := exec.Command(os.Args[0], "-test.run=^TestSuccessfulProcessPreservesDetachedDescendant$")
	parent.Env = append(os.Environ(), helperMode+"=parent",
		"KIT_GITCMD_WINDOWS_SUCCESS_MARKER="+marker)
	PrepareProcessTreeCancellation(parent, true)
	require.NoError(t, RunProcessTreeCommand(parent))
	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
}

func TestProcessTreeCancellationStopsGrandchildAfterIntermediateExits(t *testing.T) {
	const helperMode = "KIT_GITCMD_WINDOWS_TREE_HELPER"
	mode := os.Getenv(helperMode)
	marker := os.Getenv("KIT_GITCMD_WINDOWS_TREE_MARKER")
	ready := os.Getenv("KIT_GITCMD_WINDOWS_TREE_READY")
	if mode == "grandchild" {
		time.Sleep(1500 * time.Millisecond)
		_ = os.WriteFile(marker, []byte("escaped"), 0o600)
		return
	}
	if mode == "intermediate" {
		child := exec.Command(os.Args[0], "-test.run=^TestProcessTreeCancellationStopsGrandchildAfterIntermediateExits$")
		child.Env = append(os.Environ(), helperMode+"=grandchild")
		_ = child.Start()
		return
	}
	if mode == "parent" {
		intermediate := exec.Command(os.Args[0], "-test.run=^TestProcessTreeCancellationStopsGrandchildAfterIntermediateExits$")
		intermediate.Env = append(os.Environ(), helperMode+"=intermediate")
		_ = intermediate.Run()
		_ = os.WriteFile(ready, []byte("ready"), 0o600)
		time.Sleep(5 * time.Second)
		return
	}

	require := require.New(t)
	assert := assert.New(t)
	marker = filepath.Join(t.TempDir(), "escaped")
	ready = marker + ".ready"
	ctx, cancel := context.WithCancel(t.Context())
	parent := exec.CommandContext(
		ctx, os.Args[0], "-test.run=^TestProcessTreeCancellationStopsGrandchildAfterIntermediateExits$",
	)
	parent.Env = append(os.Environ(),
		helperMode+"=parent", "KIT_GITCMD_WINDOWS_TREE_MARKER="+marker,
		"KIT_GITCMD_WINDOWS_TREE_READY="+ready,
	)
	PrepareProcessTreeCancellation(parent, true)
	done := make(chan error, 1)
	go func() { done <- RunProcessTreeCommand(parent) }()
	require.Eventually(func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	_ = <-done
	time.Sleep(2 * time.Second)
	assert.NoFileExists(marker)
}
