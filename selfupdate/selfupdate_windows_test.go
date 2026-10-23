package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestInstallBinarySharingViolation(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		name := "released"
		if persistent {
			name = "persistent"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "source.exe")
			dst := filepath.Join(dir, "tool.exe")
			require.NoError(t, os.WriteFile(src, []byte("new"), 0o755))
			require.NoError(t, os.WriteFile(dst, []byte("original"), 0o755))

			handle, err := os.Open(dst)
			require.NoError(t, err)
			defer func() {
				if persistent {
					assert.NoError(t, handle.Close())
				}
			}()

			closed := make(chan error, 1)
			if !persistent {
				time.AfterFunc(100*time.Millisecond, func() { closed <- handle.Close() }) //nolint:kennlint // exercise a real Windows sharing violation until the reader closes
			}
			err = InstallBinary(src, dst)
			want := "new"
			if persistent {
				require.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)
				var renameErr *os.LinkError
				require.ErrorAs(t, err, &renameErr)
				assert.Equal(t, dst, renameErr.Old)
				require.ErrorContains(t, err, "backup: ")
				want = "original"
			} else {
				require.NoError(t, <-closed)
				require.NoError(t, err)
			}
			data, err := os.ReadFile(dst)
			require.NoError(t, err)
			assert.Equal(t, want, string(data))
			staging, err := filepath.Glob(filepath.Join(dir, ".tool.exe.*"))
			require.NoError(t, err)
			assert.Empty(t, staging)
		})
	}
}

func TestInstallBinaryWaitsForStagedReader(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.exe")
	dst := filepath.Join(dir, "tool.exe")
	require.NoError(t, os.WriteFile(src, []byte("new"), 0o755))
	require.NoError(t, os.WriteFile(dst, []byte("original"), 0o755))

	// Hold the backup move so the staged file can be opened before installation.
	destination, err := os.Open(dst)
	require.NoError(t, err)
	installed := make(chan struct{})
	var installErr error
	go func() {
		installErr = InstallBinary(src, dst)
		close(installed)
	}()
	defer func() {
		if destination != nil {
			assert.NoError(t, destination.Close())
		}
		<-installed
	}()
	var staged []string
	require.Eventually(t, func() bool {
		paths, err := filepath.Glob(filepath.Join(dir, ".tool.exe.*.new"))
		staged = paths
		return err == nil && len(staged) == 1
	}, time.Second, 5*time.Millisecond)
	reader, err := os.Open(staged[0])
	require.NoError(t, err)

	closed := make(chan error, 1)
	time.AfterFunc(100*time.Millisecond, func() { closed <- reader.Close() }) //nolint:kennlint // keep the real staged executable locked while installation retries
	require.NoError(t, destination.Close())
	destination = nil
	<-installed
	require.NoError(t, <-closed)
	require.NoError(t, installErr)
	data, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "new", string(data))
	staging, err := filepath.Glob(filepath.Join(dir, ".tool.exe.*"))
	require.NoError(t, err)
	assert.Empty(t, staging)
}
