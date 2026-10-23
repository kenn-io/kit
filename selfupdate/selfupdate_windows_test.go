package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestInstallBinarySharingViolation(t *testing.T) {
	for _, tt := range []struct {
		name    string
		wantErr bool
		want    string
	}{
		{name: "released", want: "new"},
		{name: "persistent", wantErr: true, want: "original"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "source.exe")
			dst := filepath.Join(dir, "tool.exe")
			require.NoError(t, os.WriteFile(src, []byte("new"), 0o755))
			require.NoError(t, os.WriteFile(dst, []byte("original"), 0o755))

			handle, err := os.Open(dst)
			require.NoError(t, err)
			defer func() {
				if tt.wantErr {
					assert.NoError(t, handle.Close())
				}
			}()

			closed := make(chan error, 1)
			if !tt.wantErr {
				time.AfterFunc(100*time.Millisecond, func() { closed <- handle.Close() }) //nolint:kennlint // exercise a real Windows sharing violation until the reader closes
			}
			err = InstallBinary(src, dst)
			if tt.wantErr {
				require.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)
				var renameErr *os.LinkError
				require.ErrorAs(t, err, &renameErr)
				assert.Equal(t, dst, renameErr.Old)
				require.ErrorContains(t, err, "backup: ")
			} else {
				require.NoError(t, <-closed)
				require.NoError(t, err)
			}
			data, err := os.ReadFile(dst)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(data))
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

func TestInstallBinaryWaitsForRollbackReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "source.exe")
		dst := filepath.Join(dir, "tool.exe")
		require.NoError(t, os.WriteFile(src, []byte("new"), 0o755))
		require.NoError(t, os.WriteFile(dst, []byte("original"), 0o755))

		// Block the backup move until the staged file has a reader.
		destination, err := os.Open(dst)
		require.NoError(t, err)
		defer func() { _ = destination.Close() }()
		installed := make(chan error, 1)
		go func() { installed <- InstallBinary(src, dst) }()
		synctest.Wait()
		staged, err := filepath.Glob(filepath.Join(dir, ".tool.exe.*.new"))
		require.NoError(t, err)
		require.Len(t, staged, 1)
		reader, err := os.Open(staged[0])
		require.NoError(t, err)
		defer func() { _ = reader.Close() }()
		require.NoError(t, destination.Close())

		var backups []string
		require.Eventually(t, func() bool {
			paths, err := filepath.Glob(filepath.Join(dir, ".tool.exe.*.old"))
			backups = paths
			_, statErr := os.Stat(dst)
			return err == nil && len(backups) == 1 && os.IsNotExist(statErr)
		}, time.Second, 5*time.Millisecond)
		backup, err := os.Open(backups[0])
		require.NoError(t, err)
		defer func() { _ = backup.Close() }()

		// Keep installation blocked past its budget, then let rollback finish.
		closed := make(chan error, 2)
		time.AfterFunc(2300*time.Millisecond, func() {
			closed <- reader.Close()
			closed <- backup.Close()
		})
		err = <-installed
		require.NoError(t, <-closed)
		require.NoError(t, <-closed)
		require.Error(t, err)
		assert.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)
		assert.ErrorContains(t, err, "install: ")
		assert.NotContains(t, err.Error(), "rollback also failed")
		data, err := os.ReadFile(dst)
		require.NoError(t, err)
		assert.Equal(t, "original", string(data))
		staging, err := filepath.Glob(filepath.Join(dir, ".tool.exe.*"))
		require.NoError(t, err)
		assert.Empty(t, staging)
	})
}
