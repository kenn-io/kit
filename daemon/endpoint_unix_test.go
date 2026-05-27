//go:build !windows

package daemon_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestDefaultSocketPathRejectsPublicTempFallback(t *testing.T) {
	tempDir := "/tmp"
	service := fmt.Sprintf("kitdpublic%d", os.Getpid())
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("XDG_RUNTIME_DIR", "")
	parent := filepath.Join(tempDir, fmt.Sprintf("%s-%d", service, os.Getuid()))
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	require.NoError(t, os.MkdirAll(parent, 0o700))
	require.NoError(t, os.Chmod(parent, 0o777))

	assert.Empty(t, daemon.DefaultSocketPath(service))
}

func TestDefaultSocketPathCreatesPrivateXDGRuntimeDir(t *testing.T) {
	xdg := filepath.Join("/tmp", fmt.Sprintf("kitdxdg%d", os.Getpid()))
	service := "svc"
	t.Cleanup(func() { _ = os.RemoveAll(xdg) })
	require.NoError(t, os.MkdirAll(xdg, 0o700))
	require.NoError(t, os.Chmod(xdg, 0o700))
	t.Setenv("XDG_RUNTIME_DIR", xdg)

	socketPath := daemon.DefaultSocketPath(service)
	require.NotEmpty(t, socketPath)
	parent := filepath.Join(xdg, service)
	assert.Equal(t, filepath.Join(parent, "daemon.sock"), socketPath)

	info, err := os.Stat(parent)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Zero(t, info.Mode().Perm()&0o077)
}
