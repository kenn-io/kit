//go:build windows

package daemon_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

// TestStartDetachedConsoleInputHelper is not a real test:
// TestStartDetachedDoesNotExposeConsoleInput re-executes the test binary with
// -test.run targeting it so the detached child can report whether CONIN$
// exists in its own process.
func TestStartDetachedConsoleInputHelper(t *testing.T) {
	marker := os.Getenv("KIT_DAEMON_TEST_CONSOLE_MARKER")
	if marker == "" {
		t.Skip("helper process for TestStartDetachedDoesNotExposeConsoleInput")
	}
	file, err := os.OpenFile("CONIN$", os.O_RDONLY, 0)
	if err == nil {
		_ = file.Close()
		require.NoError(t, os.WriteFile(marker, []byte("console-input-present"), 0o600))
		return
	}
	require.NoError(t, os.WriteFile(marker, []byte("console-input-unavailable"), 0o600))
}

func TestStartDetachedDoesNotExposeConsoleInput(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	marker := filepath.Join(t.TempDir(), "console-marker")
	exited := make(chan error, 1)

	err = daemon.StartDetached(t.Context(), daemon.StartDetachedOptions{
		Executable: exe,
		Args:       []string{"-test.run", "^TestStartDetachedConsoleInputHelper$"},
		Env:        append(os.Environ(), "KIT_DAEMON_TEST_CONSOLE_MARKER="+marker),
		Exited:     func(err error) { exited <- err },
	})
	require.NoError(t, err)

	// File creation precedes the marker write, so wait for the child to finish.
	select {
	case err := <-exited:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "detached child never exited")
	}

	got, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, "console-input-unavailable", strings.TrimSpace(string(got)))
}
