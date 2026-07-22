//go:build !windows

package managedworktree

import (
	"context"
	"os/exec"
)

func lifecycleCommandContext(ctx context.Context, name string) *exec.Cmd {
	return exec.CommandContext(ctx, name)
}
