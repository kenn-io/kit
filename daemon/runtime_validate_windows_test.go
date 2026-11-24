//go:build windows

package daemon_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/fslink"
	"go.kenn.io/kit/safefileio"
)

func TestRuntimeStoreValidateFiles(t *testing.T) {
	require := require.New(t)
	parent := t.TempDir()
	require.NoError(safefileio.EnsurePrivateDir(parent))
	dir := filepath.Join(parent, "runtime")
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

	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(err)
	acl, _, err := sd.DACL()
	require.NoError(err)
	require.NoError(windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
	setBroadACL(t, file)
	before = fileDACL(t, file)
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
			link := filepath.Join(store.Dir, fmt.Sprintf("daemon.%d.json", os.Getpid()))
			if kind == "junction" {
				require.NoError(t, fslink.CreateJunction(target, link))
			} else {
				err := os.Symlink(file, link)
				if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
					t.Skip("symlink privilege unavailable")
				}
				require.NoError(t, err)
			}
			child := filepath.Join(store.Dir, "clone")
			require.NoError(t, os.WriteFile(child, nil, 0o600))
			setBroadACL(t, child)
			childBefore := fileDACL(t, child)

			records, err := store.List()
			require.NoError(t, err)
			require.Len(t, records, 1)
			require.Equal(t, 123, records[0].PID)
			require.Equal(t, before, fileDACL(t, file))
			require.Equal(t, childBefore, fileDACL(t, child))
		})
	}
}

func TestRuntimeStoreValidateFilesSkipsMalformedRecordsWithoutRepair(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"invalid-json", "{"},
		{"pid-mismatch", `{"pid":998,"address":"127.0.0.1:999"}`},
		{"missing-address", `{"pid":999}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "runtime")
			store := daemon.RuntimeStore{Dir: dir, ValidateFiles: true}
			_, err := store.Write(daemon.RuntimeRecord{PID: 123, Address: "127.0.0.1:123"})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "daemon.999.json"), []byte(tc.body), 0o600))
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

func TestRuntimeStoreValidateFilesReadFailureRepairs(t *testing.T) {
	store := daemon.RuntimeStore{Dir: filepath.Join(t.TempDir(), "runtime"), ValidateFiles: true}
	_, err := store.Write(daemon.RuntimeRecord{PID: 123, Address: "127.0.0.1:123"})
	require.NoError(t, err)
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
	child := filepath.Join(store.Dir, "clone")
	require.NoError(t, os.WriteFile(child, nil, 0o600))
	setBroadACL(t, child)
	before := fileDACL(t, child)

	records, err := store.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, 123, records[0].PID)
	require.NotEqual(t, before, fileDACL(t, child))
	store.ValidateFiles = false
	defaultRecords, err := store.List()
	require.NoError(t, err)
	require.Equal(t, defaultRecords, records)
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
