//go:build unix

package agenthook

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCodexResourceErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		transient bool
	}{
		{"pipe descriptor limit", &os.SyscallError{Syscall: "pipe2", Err: syscall.EMFILE}, true},
		{"system descriptor limit", &os.SyscallError{Syscall: "pipe2", Err: syscall.ENFILE}, true},
		{"memory", &os.PathError{Op: "fork/exec", Err: syscall.ENOMEM}, true},
		{"temporary resource", &os.PathError{Op: "fork/exec", Err: syscall.EAGAIN}, true},
		{"busy executable", &os.PathError{Op: "fork/exec", Err: syscall.ETXTBSY}, true},
		{"permission", &os.PathError{Op: "fork/exec", Err: os.ErrPermission}, false},
		{"missing executable", exec.ErrNotFound, false},
		{"bad executable", &os.PathError{Op: "fork/exec", Err: syscall.ENOEXEC}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.transient, transientCodexError(fmt.Errorf("startup: %w", tc.err)))
		})
	}
}
