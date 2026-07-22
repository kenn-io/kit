//go:build windows

package managedworktree

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func lifecycleCommandContext(ctx context.Context, name string) *exec.Cmd {
	if lifecycleShebangScript(name) {
		if shell, err := exec.LookPath("sh"); err == nil {
			return exec.CommandContext(ctx, shell, name)
		}
	}
	return exec.CommandContext(ctx, name)
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
