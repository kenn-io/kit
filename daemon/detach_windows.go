//go:build windows

package daemon

import (
	"os/exec"
	"syscall"
)

// createNoWindow (CREATE_NO_WINDOW) runs the child on a hidden console
// instead of no console. A DETACHED_PROCESS child has no console, so every
// console-subsystem descendant it spawns (git, taskkill, agent CLIs, ...)
// allocates a new visible console window. A hidden console is inherited by
// descendants, so background daemons never flash terminal windows. The two
// flags are mutually exclusive: CREATE_NO_WINDOW is ignored when
// DETACHED_PROCESS is set.
const createNoWindow = 0x08000000

func detachChild(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow
}
