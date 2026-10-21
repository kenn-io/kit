//go:build windows

package consolewindow

import (
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestHide(t *testing.T) {
	for _, tt := range []struct {
		name string
		attr *syscall.SysProcAttr
	}{
		{name: "fresh command"},
		{name: "existing attributes", attr: &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "unused")
			cmd.SysProcAttr = tt.attr
			want := syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
			if tt.attr != nil {
				want = *tt.attr
				want.CreationFlags |= windows.CREATE_NO_WINDOW
			}

			Hide(cmd)

			require.NotNil(t, cmd.SysProcAttr)
			assert.Equal(t, want, *cmd.SysProcAttr)
		})
	}
}
