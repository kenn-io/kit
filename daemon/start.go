package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// StartDetachedOptions configures StartDetached.
type StartDetachedOptions struct {
	Executable      string
	Args            []string
	Dir             string
	Env             []string
	Stdout          io.Writer
	Stderr          io.Writer
	RefuseEphemeral bool
	AfterStart      func(*exec.Cmd)
}

// StartDetached starts a child process detached from the caller's process
// group where the platform supports it. On Windows the child runs on its own
// hidden console so neither it nor its console-subsystem descendants open
// visible console windows.
func StartDetached(ctx context.Context, opts StartDetachedOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	exe := opts.Executable
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return err
		}
	}
	if opts.RefuseEphemeral && IsEphemeralExecutable(exe) {
		return fmt.Errorf("refusing to auto-start daemon from ephemeral binary %s", filepath.Base(exe))
	}
	cmd := exec.Command(exe, opts.Args...)
	cmd.Dir = opts.Dir
	if opts.Env != nil {
		cmd.Env = opts.Env
	}
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	detachChild(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start detached daemon: %w", err)
	}
	if opts.AfterStart != nil {
		opts.AfterStart(cmd)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// IsEphemeralExecutable reports whether exe looks like a test or go-build
// binary that should not be used as a long-lived daemon.
func IsEphemeralExecutable(exe string) bool {
	base := filepath.Base(exe)
	lowerBase := strings.ToLower(base)
	if withoutExe, ok := strings.CutSuffix(lowerBase, ".exe"); ok {
		lowerBase = withoutExe
	}
	normalized := strings.ReplaceAll(exe, `\`, `/`)
	return strings.HasSuffix(lowerBase, ".test") || strings.Contains(normalized, "/go-build")
}
