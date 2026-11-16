package selfupdate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Other goroutines can start processes while an update writes its staged copy.
// Each install must be able to execute that copy after closing it.
func TestInstallBinaryWhileStartingProcesses(t *testing.T) {
	t.Parallel()
	src := filepath.Join(t.TempDir(), "source")
	require.NoError(t, os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	for range 16 {
		t.Run("installer", func(t *testing.T) {
			t.Parallel()
			dst := filepath.Join(t.TempDir(), "installed")
			for range 50 {
				err := installBinary(src, dst, func(path string) error {
					return runNewBinary(t.Context(), path, nil)
				})
				require.NoError(t, err)
			}
		})
	}
}
