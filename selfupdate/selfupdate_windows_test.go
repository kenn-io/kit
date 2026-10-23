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

			// Match a reader that allows reads and writes but prevents renaming.
			path, err := windows.UTF16PtrFromString(dst)
			require.NoError(t, err)
			handle, err := windows.CreateFile(path, windows.GENERIC_READ,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
				windows.FILE_ATTRIBUTE_NORMAL, 0)
			require.NoError(t, err)
			defer func() {
				if persistent {
					assert.NoError(t, windows.CloseHandle(handle))
				}
			}()

			closed := make(chan error, 1)
			if !persistent {
				time.AfterFunc(100*time.Millisecond, func() { closed <- windows.CloseHandle(handle) }) //nolint:kennlint // exercise a real Windows sharing violation until the reader closes
			}
			started := time.Now()
			err = InstallBinary(src, dst)
			elapsed := time.Since(started)
			want := "new"
			if persistent {
				require.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)
				var renameErr *os.LinkError
				require.ErrorAs(t, err, &renameErr)
				assert.Equal(t, dst, renameErr.Old)
				require.ErrorContains(t, err, "backup: ")
				assert.GreaterOrEqual(t, elapsed, 2*time.Second)
				assert.Less(t, elapsed, 5*time.Second)
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
