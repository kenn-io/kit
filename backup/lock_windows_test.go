//go:build windows

package backup

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// openLikeReader opens path the way os.ReadFile does: without delete sharing.
func openLikeReader(t *testing.T, path string) windows.Handle {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	return handle
}

func TestReleaseWithLockFileReader(t *testing.T) {
	for _, tt := range []struct {
		name    string
		wantErr bool
	}{
		{name: "held past timeout", wantErr: true},
		{name: "released within timeout"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			if tt.wantErr {
				oldTimeout := claimBusyTimeout
				claimBusyTimeout = 20 * time.Millisecond
				t.Cleanup(func() { claimBusyTimeout = oldTimeout })
			}

			shared, err := initTestRepo(t).AcquireSharedLock("verify", false)
			require.NoError(err)
			handle := openLikeReader(t, shared.path)
			if tt.wantErr {
				require.ErrorIs(shared.Release(), windows.ERROR_SHARING_VIOLATION)
				require.FileExists(shared.path)
				require.NoError(windows.CloseHandle(handle))
				require.NoError(shared.Release())
			} else {
				closed := make(chan error, 1)
				time.AfterFunc(50*time.Millisecond, func() { closed <- windows.CloseHandle(handle) }) //nolint:kennlint // keeps a real Windows reader handle open while Release retries; the OS sharing violation can't run in a bubble
				require.NoError(shared.Release())
				require.NoError(<-closed)
			}
			require.NoFileExists(shared.path)
		})
	}
}
