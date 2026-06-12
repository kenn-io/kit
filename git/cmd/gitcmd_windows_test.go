//go:build windows

package gitcmd

import (
	"context"
	"testing"

	"golang.org/x/sys/windows"
)

func TestRunnerCommandHidesGitConsoleWindowOnWindows(t *testing.T) {
	cmd := New().Command(context.Background(), "", "status")

	if cmd.SysProcAttr == nil {
		t.Fatal("git command SysProcAttr is nil")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("git command creation flags = %#x, want CREATE_NO_WINDOW", cmd.SysProcAttr.CreationFlags)
	}
}
