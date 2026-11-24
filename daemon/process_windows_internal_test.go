//go:build windows

package daemon

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestProcessAliveOpenFailures(t *testing.T) {
	original := openProcessForLiveness
	t.Cleanup(func() { openProcessForLiveness = original })
	for _, tc := range []struct {
		name  string
		err   error
		alive bool
		dead  bool
	}{
		{name: "invalid PID", err: windows.ERROR_INVALID_PARAMETER, dead: true},
		{name: "access denied", err: syscall.ERROR_ACCESS_DENIED, alive: true},
		{name: "unknown failure", err: windows.ERROR_NOT_ENOUGH_MEMORY},
	} {
		t.Run(tc.name, func(t *testing.T) {
			openProcessForLiveness = func(uint32, bool, uint32) (syscall.Handle, error) { return 0, tc.err }
			require.Equal(t, tc.alive, ProcessAlive(123))
			require.Equal(t, tc.dead, processConfirmedDead(123))
		})
	}
}
