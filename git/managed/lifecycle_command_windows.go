//go:build windows

package managedworktree

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
)

func lifecycleCommandContext(ctx context.Context, name string) *exec.Cmd {
	var cmd *exec.Cmd
	if lifecycleShebangScript(name) {
		if shell, err := exec.LookPath("sh"); err == nil {
			cmd = exec.CommandContext(ctx, shell, name)
		}
	}
	if cmd == nil {
		cmd = exec.CommandContext(ctx, name)
	}
	gitcmd.PrepareProcessTreeCancellation(cmd, true)
	return cmd
}

// lifecycleShebangScript mirrors Middleman's production Windows command
// resolution: only extensionless path-like commands opt into shebang handling.
func lifecycleShebangScript(path string) bool {
	if !filepath.IsAbs(path) && !strings.ContainsAny(path, `\/`) {
		return false
	}
	if filepath.Ext(path) != "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var header [2]byte
	n, err := io.ReadFull(f, header[:])
	return err == nil && n == len(header) && header[0] == '#' && header[1] == '!'
}
