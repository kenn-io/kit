//go:build windows

package daemon_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/fslink"
	"go.kenn.io/kit/safefileio"
)

func TestRuntimeStoreValidateFiles(t *testing.T) {
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), "runtime")
	require.NoError(safefileio.EnsurePrivateDir(dir))
	store := daemon.RuntimeStore{Dir: dir, ValidateFiles: true}
	_, err := store.Write(daemon.RuntimeRecord{PID: 123, Address: "127.0.0.1:123"})
	require.NoError(err)
	lock, err := store.LockPath()
	require.NoError(err)
	require.NoError(os.WriteFile(lock, nil, 0o600))

	child := filepath.Join(dir, "clone")
	require.NoError(os.Mkdir(child, 0o700))
	for i := range 100 {
		require.NoError(os.WriteFile(filepath.Join(child, fmt.Sprint(i)), nil, 0o600))
	}
	file := filepath.Join(child, "0")
	setBroadACL(t, file)
	before := fileDACL(t, file)
	records, err := store.List()
	require.NoError(err)
	require.Len(records, 1)
	_, err = store.LockPath()
	require.NoError(err)
	require.Equal(before, fileDACL(t, file))

	store.ValidateFiles = false
	_, err = store.List()
	require.NoError(err)
	require.NotEqual(before, fileDACL(t, file))

	store.ValidateFiles = true
	setBroadACL(t, file)
	before = fileDACL(t, file)
	setBroadACL(t, filepath.Join(dir, "daemon.123.json"))
	records, err = store.List()
	require.NoError(err)
	require.Len(records, 1)
	require.NotEqual(before, fileDACL(t, file))

	setBroadACL(t, file)
	before = fileDACL(t, file)
	setBroadACL(t, lock)
	_, err = store.LockPath()
	require.NoError(err)
	require.NotEqual(before, fileDACL(t, file))

	setBroadACL(t, file)
	before = fileDACL(t, file)
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(err)
	acl, _, err := sd.DACL()
	require.NoError(err)
	require.NoError(windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
	_, err = store.List()
	require.NoError(err)
	require.NotEqual(before, fileDACL(t, file))
}

func TestRuntimeStoreValidateFilesRejectsJunction(t *testing.T) {
	require := require.New(t)
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	require.NoError(os.Mkdir(target, 0o700))
	link := filepath.Join(parent, "link")
	require.NoError(fslink.CreateJunction(target, link))
	store := daemon.RuntimeStore{Dir: link, ValidateFiles: true}
	_, err := store.List()
	require.Error(err)
	_, err = store.LockPath()
	require.Error(err)
}

func TestRuntimeStoreValidateFilesSkipsLinkedRecords(t *testing.T) {
	for _, kind := range []string{"symlink", "junction"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			store := daemon.RuntimeStore{Dir: filepath.Join(base, "runtime"), ValidateFiles: true}
			_, err := store.Write(daemon.RuntimeRecord{PID: 123, Address: "127.0.0.1:123"})
			require.NoError(t, err)
			target := filepath.Join(base, "target")
			require.NoError(t, os.Mkdir(target, 0o700))
			file := filepath.Join(target, "record.json")
			require.NoError(t, os.WriteFile(file, []byte(`{"pid":999,"address":"127.0.0.1:999"}`), 0o600))
			setBroadACL(t, file)
			before := fileDACL(t, file)
			link := filepath.Join(store.Dir, "daemon.999.json")
			if kind == "junction" {
				require.NoError(t, fslink.CreateJunction(target, link))
			} else {
				err := os.Symlink(file, link)
				if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
					t.Skip("symlink privilege unavailable")
				}
				require.NoError(t, err)
			}

			records, err := store.List()
			require.NoError(t, err)
			require.Len(t, records, 1)
			require.Equal(t, 123, records[0].PID)
			require.Equal(t, before, fileDACL(t, file))
		})
	}
}

func TestRuntimeStoreValidateFilesSkipsMalformedRecordsWithoutRepair(t *testing.T) {
	for _, body := range []string{"{", `{"pid":998,"address":"127.0.0.1:999"}`, `{"pid":999}`} {
		t.Run(body, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "runtime")
			store := daemon.RuntimeStore{Dir: dir, ValidateFiles: true}
			_, err := store.Write(daemon.RuntimeRecord{PID: 123, Address: "127.0.0.1:123"})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "daemon.999.json"), []byte(body), 0o600))
			child := filepath.Join(dir, "clone")
			require.NoError(t, os.WriteFile(child, nil, 0o600))
			setBroadACL(t, child)
			before := fileDACL(t, child)

			records, err := store.List()
			require.NoError(t, err)
			require.Len(t, records, 1)
			require.Equal(t, 123, records[0].PID)
			require.Equal(t, before, fileDACL(t, child))
		})
	}
}

func TestRuntimeStoreValidateFilesUnrepairableRecords(t *testing.T) {
	for _, tc := range []struct {
		name string
		pid  int
		live bool
	}{
		{name: "dead", pid: deadPID(t)},
		{name: "live", pid: os.Getpid(), live: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := daemon.RuntimeStore{Dir: filepath.Join(t.TempDir(), "runtime"), ValidateFiles: true}
			_, err := store.Write(daemon.RuntimeRecord{PID: 123, Address: "127.0.0.1:123"})
			require.NoError(t, err)
			path, err := store.Write(daemon.RuntimeRecord{PID: tc.pid, Address: "127.0.0.1:123"})
			require.NoError(t, err)
			setBroadACL(t, path)
			sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
			require.NoError(t, err)
			acl, _, err := sd.DACL()
			require.NoError(t, err)
			require.NoError(t, windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
			child := filepath.Join(store.Dir, "clone")
			require.NoError(t, os.WriteFile(child, nil, 0o600))
			setBroadACL(t, child)
			before := fileDACL(t, child)

			records, err := store.List()
			if tc.live {
				require.Error(t, err)
				_, _, found, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{RequirePIDAlive: true})
				require.Error(t, err)
				require.False(t, found)
			} else {
				require.NoError(t, err)
				require.Len(t, records, 1)
				require.Equal(t, 123, records[0].PID)
			}
			require.NotEqual(t, before, fileDACL(t, child))
			require.FileExists(t, path)
		})
	}
}

func TestRuntimeStoreValidateFilesUnreadableRecords(t *testing.T) {
	for _, pid := range []int{deadPID(t), os.Getpid()} {
		t.Run(fmt.Sprint(pid), func(t *testing.T) {
			store := daemon.RuntimeStore{Dir: filepath.Join(t.TempDir(), "runtime"), ValidateFiles: true}
			path, err := store.Write(daemon.RuntimeRecord{PID: pid, Address: "127.0.0.1:123"})
			require.NoError(t, err)
			name, err := windows.UTF16PtrFromString(path)
			require.NoError(t, err)
			handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, 0, 0)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, windows.CloseHandle(handle)) })
			defaultStore := store
			defaultStore.ValidateFiles = false
			records, err := defaultStore.List()
			require.NoError(t, err)
			require.Empty(t, records)
			child := filepath.Join(store.Dir, "clone")
			require.NoError(t, os.WriteFile(child, nil, 0o600))
			setBroadACL(t, child)
			before := fileDACL(t, child)
			records, err = store.List()
			if pid == os.Getpid() {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Empty(t, records)
			}
			require.NotEqual(t, before, fileDACL(t, child))
			_, _, found, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{RequirePIDAlive: true})
			if pid == os.Getpid() {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.False(t, found)
		})
	}
}

func TestRuntimeStoreValidateFilesSkipsRemovedRecords(t *testing.T) {
	store := daemon.RuntimeStore{Dir: filepath.Join(t.TempDir(), "runtime"), ValidateFiles: true}
	_, err := store.Write(daemon.RuntimeRecord{PID: 123, Address: "127.0.0.1:123"})
	require.NoError(t, err)
	stop := make(chan struct{})
	done := make(chan error, 1)
	path := filepath.Join(store.Dir, "daemon.999.json")
	go func() {
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
				done <- err
				return
			}
			if err := os.Remove(path); err != nil {
				done <- err
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			require.Fail(t, "record writer did not stop")
		}
	})
	for range 300 {
		records, err := store.List()
		require.NoError(t, err)
		require.Len(t, records, 1)
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
