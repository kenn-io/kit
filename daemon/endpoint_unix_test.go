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
