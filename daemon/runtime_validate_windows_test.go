//go:build windows

package daemon_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/fslink"
	"go.kenn.io/kit/safefileio"
)

func TestRuntimeStoreValidateFilesWithoutRepair(t *testing.T) {
	for _, name := range []string{"valid", "symlink", "junction", "invalid-json", "pid-mismatch", "missing-address"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			store := daemon.RuntimeStore{Dir: filepath.Join(base, "runtime"), ValidateFiles: true}
			_, err := store.Write(daemon.RuntimeRecord{PID: 123, Address: "127.0.0.1:123"})
			require.NoError(t, err)
			lock, err := store.LockPath()
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(lock, nil, 0o600))
			path := filepath.Join(store.Dir, "daemon.999.json")
			switch name {
			case "symlink", "junction":
				path = filepath.Join(store.Dir, fmt.Sprintf("daemon.%d.json", os.Getpid()))
				target := filepath.Join(base, "target")
				if name == "junction" {
					require.NoError(t, os.Mkdir(target, 0o700))
					require.NoError(t, fslink.CreateJunction(target, path))
				} else {
					require.NoError(t, os.WriteFile(target, []byte("{}"), 0o600))
					err := os.Symlink(target, path)
					if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
						t.Skip("symlink privilege unavailable")
					}
					require.NoError(t, err)
				}
			case "invalid-json":
				require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
			case "pid-mismatch":
				require.NoError(t, os.WriteFile(path, []byte(`{"pid":998,"address":"127.0.0.1:999"}`), 0o600))
			case "missing-address":
				require.NoError(t, os.WriteFile(path, []byte(`{"pid":999}`), 0o600))
			}
			child := filepath.Join(store.Dir, "clone")
			require.NoError(t, os.WriteFile(child, nil, 0o600))
			setBroadACL(t, child)
			before := fileDACL(t, child)

			records, err := store.List()
			require.NoError(t, err)
			require.Len(t, records, 1)
			require.Equal(t, 123, records[0].PID)
			if name == "valid" {
				_, err = store.LockPath()
				require.NoError(t, err)
			}
			require.Equal(t, before, fileDACL(t, child))
		})
	}
}

func TestRuntimeStoreValidateFilesFallsBackToRepair(t *testing.T) {
	for _, name := range []string{"record-dacl", "lock-dacl", "directory-dacl", "read-failure"} {
		t.Run(name, func(t *testing.T) {
			store := daemon.RuntimeStore{Dir: filepath.Join(t.TempDir(), "runtime"), ValidateFiles: true}
			path, err := store.Write(daemon.RuntimeRecord{PID: 123, Address: "127.0.0.1:123"})
			require.NoError(t, err)
			switch name {
			case "record-dacl":
				setBroadACL(t, path)
			case "lock-dacl":
				lock, err := store.LockPath()
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(lock, nil, 0o600))
				setBroadACL(t, lock)
			case "directory-dacl":
				// Keep inherited record permissions trusted so only the directory check triggers repair.
				require.NoError(t, safefileio.EnsurePrivateDir(filepath.Dir(store.Dir)))
				sd, err := windows.GetNamedSecurityInfo(store.Dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
				require.NoError(t, err)
				acl, _, err := sd.DACL()
				require.NoError(t, err)
				require.NoError(t, windows.SetNamedSecurityInfo(store.Dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
			case "read-failure":
				path, err := store.Write(daemon.RuntimeRecord{PID: os.Getpid(), Address: "127.0.0.1:456"})
				require.NoError(t, err)
				locked, err := os.OpenFile(path, os.O_RDWR, 0)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, locked.Close()) })
				var overlapped windows.Overlapped
				require.NoError(t, windows.LockFileEx(windows.Handle(locked.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped))
				t.Cleanup(func() { require.NoError(t, windows.UnlockFileEx(windows.Handle(locked.Fd()), 0, 1, 0, &overlapped)) })
				file, err := safefileio.OpenCurrentUserFile(path)
				require.NoError(t, err)
				require.NoError(t, safefileio.ValidateTrustedFile(file))
				_, err = file.Read(make([]byte, 1))
				require.ErrorIs(t, err, windows.ERROR_LOCK_VIOLATION)
				require.NoError(t, file.Close())
			}
			child := filepath.Join(store.Dir, "clone")
			require.NoError(t, os.WriteFile(child, nil, 0o600))
			setBroadACL(t, child)
			before := fileDACL(t, child)

			if name == "lock-dacl" {
				_, err = store.LockPath()
				require.NoError(t, err)
			} else {
				records, err := store.List()
				require.NoError(t, err)
				require.Len(t, records, 1)
				require.Equal(t, 123, records[0].PID)
			}
			require.NotEqual(t, before, fileDACL(t, child))
		})
	}
}

func TestRuntimeStoreStartLockPreparation(t *testing.T) {
	for _, operation := range []string{"acquire", "try", "owner", "listen-explicit", "listen-derived"} {
		for _, mode := range []string{"default", "validated", "unsafe-lock"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				store := daemon.RuntimeStore{Dir: filepath.Join(t.TempDir(), "runtime"), ValidateFiles: mode != "default"}
				var socket string
				if operation == "listen-explicit" || operation == "listen-derived" {
					socket = unixSocketPath(t)
					store.Dir = filepath.Dir(socket)
				}
				path, err := store.LockPath()
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, nil, 0o600))
				if mode == "unsafe-lock" {
					setBroadACL(t, path)
				}
				child := filepath.Join(store.Dir, "clone")
				require.NoError(t, os.WriteFile(child, nil, 0o600))
				setBroadACL(t, child)
				before := fileDACL(t, child)

				switch operation {
				case "acquire":
					release, err := store.AcquireStartLock(t.Context())
					require.NoError(t, err)
					t.Cleanup(release)
				case "try":
					release, acquired, err := store.TryAcquireStartLock(t.Context())
					require.NoError(t, err)
					require.True(t, acquired)
					t.Cleanup(release)
				case "owner":
					release, err := store.AcquireOwnerLock(t.Context())
					require.NoError(t, err)
					t.Cleanup(release)
				default:
					var options []daemon.ListenOption
					if operation == "listen-explicit" {
						options = append(options, daemon.WithListenLockPath(filepath.Join(store.Dir, "explicit.lock")))
					}
					listener, err := daemon.Listen(t.Context(), daemon.Endpoint{Network: daemon.NetworkUnix, Address: socket}, options...)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, listener.Close()) })
				}
				if mode == "validated" && (operation == "acquire" || operation == "try") {
					require.Equal(t, before, fileDACL(t, child))
				} else {
					require.NotEqual(t, before, fileDACL(t, child))
				}
				if operation == "acquire" || operation == "try" {
					legacy := flock.New(path)
					t.Cleanup(func() { require.NoError(t, legacy.Close()) })
					acquired, err := legacy.TryLock()
					require.NoError(t, err)
					require.False(t, acquired)
				}
			})
		}
	}
}

func setBroadACL(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	source := filepath.Join(filepath.Dir(path), "broad-source")
	require.NoError(t, os.Mkdir(source, 0o700))
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + tokenUser.User.Sid.String() + ")(A;OICI;GR;;;WD)")
	require.NoError(t, err)
	acl, _, err := sd.DACL()
	require.NoError(t, err)
	require.NoError(t, windows.SetNamedSecurityInfo(source, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
	moved := filepath.Join(source, "file")
	require.NoError(t, os.WriteFile(moved, body, 0o600))
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Rename(moved, path))
	require.NoError(t, os.Remove(source))
}

func fileDACL(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	return sd.String()
}
