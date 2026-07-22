//go:build !windows

package managedworktree

import (
	"context"
	"os/exec"

	gitcmd "go.kenn.io/kit/git/cmd"
)

func lifecycleCommandContext(ctx context.Context, name string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name)
	gitcmd.PrepareProcessTreeCancellation(cmd, false)
	return cmd
}
