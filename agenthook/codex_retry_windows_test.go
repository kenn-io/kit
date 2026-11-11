package agenthook

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/windows"
)

func TestCodexResourceErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		transient bool
	}{
		{"open files", &os.SyscallError{Syscall: "pipe", Err: windows.ERROR_TOO_MANY_OPEN_FILES}, true},
		{"memory", &os.PathError{Op: "fork/exec", Err: windows.ERROR_NOT_ENOUGH_MEMORY}, true},
		{"out of memory", &os.SyscallError{Syscall: "pipe", Err: windows.ERROR_OUTOFMEMORY}, true},
		{"system resources", &os.PathError{Op: "fork/exec", Err: windows.ERROR_NO_SYSTEM_RESOURCES}, true},
		{"commitment limit", &os.PathError{Op: "fork/exec", Err: windows.ERROR_COMMITMENT_LIMIT}, true},
		{"sharing violation", &os.PathError{Op: "fork/exec", Err: windows.ERROR_SHARING_VIOLATION}, true},
		{"permission", &os.PathError{Op: "fork/exec", Err: os.ErrPermission}, false},
		{"missing executable", exec.ErrNotFound, false},
		{"bad executable", &os.PathError{Op: "fork/exec", Err: windows.ERROR_BAD_EXE_FORMAT}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.transient, transientCodexError(fmt.Errorf("startup: %w", tc.err)))
		})
	}
}
