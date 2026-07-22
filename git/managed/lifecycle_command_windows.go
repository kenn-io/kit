//go:build windows

package managedworktree

import (
	"context"
	"os/exec"

	gitcmd "go.kenn.io/kit/git/cmd"
)

func lifecycleCommandContext(ctx context.Context, name string) *exec.Cmd {
	var cmd *exec.Cmd
	if interpreter, args, ok := lifecycleShebangCommand(name); ok {
		cmd = exec.CommandContext(ctx, interpreter, args...)
	}
	if cmd == nil {
		cmd = exec.CommandContext(ctx, name)
	}
	gitcmd.PrepareProcessTreeCancellation(cmd, true)
	return cmd
}
