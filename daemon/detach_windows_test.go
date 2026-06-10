//go:build windows

package daemon

import (
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

// detachedProcess (DETACHED_PROCESS) must never come back: it is mutually
// exclusive with CREATE_NO_WINDOW, and a child with no console makes every
// console-subsystem descendant open a visible console window.
const detachedProcess = 0x00000008

func TestDetachChildUsesHiddenConsole(t *testing.T) {
	assert := assert.New(t)

	cmd := exec.Command("cmd.exe")
	detachChild(cmd)

	flags := cmd.SysProcAttr.CreationFlags
	assert.NotZero(flags&syscall.CREATE_NEW_PROCESS_GROUP, "child must run in its own process group")
	assert.NotZero(flags&createNoWindow, "child must run on a hidden console")
	assert.Zero(flags&detachedProcess, "DETACHED_PROCESS would make console descendants open visible windows")
}

func TestDetachChildPreservesExistingSysProcAttr(t *testing.T) {
	assert := assert.New(t)

	const createSuspended = 0x00000004
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended}
	detachChild(cmd)

	flags := cmd.SysProcAttr.CreationFlags
	assert.NotZero(flags&createSuspended, "caller-set creation flags must be preserved")
	assert.NotZero(flags & createNoWindow)
}
