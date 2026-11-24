//go:build windows

package daemon_test

import (
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
