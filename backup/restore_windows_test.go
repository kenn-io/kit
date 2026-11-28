//go:build windows

package backup

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestRestoreOverwriteIgnoresUnlistableUnrelatedDirectory(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(t.Context(), r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	target := t.TempDir()
	unrelated := filepath.Join(target, "unrelated")
	require.NoError(os.Mkdir(unrelated, 0o700))
	denyWindowsDirectoryListing(t, unrelated)
	_, err = os.ReadDir(unrelated)
	require.ErrorIs(err, fs.ErrPermission)

	_, err = Restore(t.Context(), r, newTestApp(), RestoreOptions{TargetDir: target, Overwrite: true})
	require.NoError(err)
}

func denyWindowsDirectoryListing(t *testing.T, path string) {
	t.Helper()
	require := require.New(t)
	name, err := windows.UTF16PtrFromString(path)
	require.NoError(err)
	handle, err := windows.CreateFile(name, windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(windows.CloseHandle(handle)) })
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(err)
	original, _, err := sd.DACL()
	require.NoError(err)
	control, _, err := sd.Control()
	require.NoError(err)
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		info = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	t.Cleanup(func() {
		require.NoError(windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, info, nil, nil, original, nil))
	})
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(err)
	var pin runtime.Pinner
	pin.Pin(user.User.Sid)
	defer pin.Unpin()
	trustee := windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid)}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		{AccessPermissions: windows.FILE_LIST_DIRECTORY, AccessMode: windows.DENY_ACCESS, Inheritance: windows.NO_INHERITANCE, Trustee: trustee},
		{AccessPermissions: windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.FILE_GENERIC_EXECUTE | windows.READ_CONTROL | windows.WRITE_DAC | windows.DELETE, AccessMode: windows.GRANT_ACCESS, Inheritance: windows.NO_INHERITANCE, Trustee: trustee},
	}, nil)
	require.NoError(err)
	require.NoError(windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
}
