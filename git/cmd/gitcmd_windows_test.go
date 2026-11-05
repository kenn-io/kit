//go:build windows

package gitcmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/kit/git/internal/shellquote"
)

func TestRunnerCancellationAfterIntermediateExit(t *testing.T) {
	started := make(chan struct{})
	disconnected := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			close(disconnected)
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	executable, err := os.Executable()
	require.NoError(t, err)
	// The trailing command keeps Git's shell alive while the helper waits.
	runner := New().WithConfig("alias.hold-pipe", "!"+shellquote.Single(executable)+" -test.run=^TestRunnerHeldPipeHelper$; :")
	runner.DisableSafeDirectoryForward = true
	runner.Env = append(runner.Env, "GITCMD_PIPE_HELPER_URL="+server.URL)
	// The orphan can outlive fixture cleanup and creates no files here.
	dir := os.TempDir() //nolint:usetesting // An intentionally orphaned helper must not hold a fixture directory open.
	cmd := runner.Command(t.Context(), dir, "hold-pipe")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	require.NoError(t, cmd.Start())
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Cancel()
			_ = cmd.Wait()
		}
	}()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "Git's HTTP helper did not start")
	}
	children, err := snapshotGitProcesses()
	require.NoError(t, err)
	rootPID := uint32(cmd.Process.Pid)
	var rootCreated int64
	for _, child := range children[uint32(os.Getpid())] {
		if child.pid == rootPID {
			rootCreated = child.created
		}
	}
	require.NotZero(t, rootCreated)
	require.Len(t, children[rootPID], 1)
	middle := children[rootPID][0]
	require.NotEmpty(t, children[middle.pid], "the shell must have a live HTTP helper")
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, middle.pid)
	require.NoError(t, err)
	defer func() {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
	}()

	// A different creation time must preserve the live process at this PID.
	replacement := middle
	replacement.created++
	require.NoError(t, killGitChild(replacement))
	state, err := windows.WaitForSingleObject(handle, 1000)
	require.NoError(t, err)
	require.Equal(t, uint32(windows.WAIT_TIMEOUT), state)

	require.NoError(t, windows.TerminateProcess(handle, 1))
	require.Error(t, cmd.Wait())
	require.NoError(t, windows.CloseHandle(handle))
	handle = 0
	select {
	case <-disconnected:
		require.FailNow(t, "HTTP helper exited before cancellation reached it")
	default:
	}
	require.NoError(t, killGitChildren(rootPID, rootCreated, children))
	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		require.FailNow(t, "HTTP helper survived its intermediate process")
	}
}

func TestRunnerCommandHidesGitConsoleWindow(t *testing.T) {
	cmd := New().Command(t.Context(), "", "status")

	require.NotNil(t, cmd.SysProcAttr)
	assert.NotZero(t, cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW, "git subprocesses must not flash console windows")
}

func TestRunnerCommandAllowsConsoleWindowForTerminalPrompts(t *testing.T) {
	runner := New()
	runner.TerminalPrompt = true

	cmd := runner.Command(t.Context(), "", "fetch")

	if cmd.SysProcAttr != nil {
		assert.Zero(t, cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW, "interactive git prompts should be able to use the console")
	}
}
