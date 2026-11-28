// Package binaryprobe runs a binary with bounded output and execution time.
package binaryprobe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kit/consolewindow"
)

const (
	runTimeout  = time.Minute
	waitDelay   = 5 * time.Second
	outputLimit = 4 << 10
	stdoutLimit = 16 << 10
)

// Run executes path with args, returning stdout separately from diagnostics.
func Run(ctx context.Context, path string, args []string) ([]byte, error) {
	return run(ctx, path, args, true)
}

// RunDiscard executes a binary without requiring its stdout to fit the limit.
func RunDiscard(ctx context.Context, path string, args []string) error {
	_, err := run(ctx, path, args, false)
	return err
}

func run(ctx context.Context, path string, args []string, requireStdout bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := consolewindow.CommandContext(ctx, path, args...)
	// Descendants may hold the pipes open after the binary exits.
	cmd.WaitDelay = waitDelay
	diagnostic := &outputPrefix{limit: outputLimit}
	var stdout *outputPrefix
	cmd.Stdout = diagnostic
	if requireStdout {
		stdout = &outputPrefix{limit: stdoutLimit}
		cmd.Stdout = io.MultiWriter(stdout, diagnostic)
	}
	cmd.Stderr = diagnostic
	err := cmd.Run()
	// Wait can report success if cancellation occurs after exit while reading.
	switch ctxErr := ctx.Err(); {
	case ctxErr == nil:
	case err == nil:
		err = ctxErr
	case !errors.Is(err, ctxErr):
		err = fmt.Errorf("%w: %w", ctxErr, err)
	}
	if err != nil {
		if text := diagnostic.String(); text != "" {
			return nil, fmt.Errorf("%w: %s", err, text)
		}
		return nil, err
	}
	if stdout == nil {
		return nil, nil
	}
	if stdout.truncated {
		return nil, errors.New("stdout exceeds 16 KiB limit")
	}
	return stdout.buf, nil
}

type outputPrefix struct {
	mu        sync.Mutex
	buf       []byte
	truncated bool
	limit     int
}

func (w *outputPrefix) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := min(len(p), w.limit-len(w.buf))
	w.buf = append(w.buf, p[:n]...)
	w.truncated = w.truncated || n < len(p)
	return len(p), nil
}

func (w *outputPrefix) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	text := strings.TrimSpace(string(w.buf))
	if w.truncated {
		text += "..."
	}
	return text
}
