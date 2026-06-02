//go:build windows

package safefileio

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestEnsurePrivateDirCreatesOwnedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime")

	require.NoError(t, EnsurePrivateDir(dir))

	ownerSID, err := currentWindowsOwnerSID()
	require.NoError(t, err)
	descriptor, err := windows.GetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	require.NoError(t, err)
	owner, _, err := descriptor.Owner()
	require.NoError(t, err)
	require.NotNil(t, owner)
	require.True(t, owner.Equals(ownerSID))
}

func TestValidatePrivateDirAcceptsPrivateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime")
	require.NoError(t, EnsurePrivateDir(dir))

	require.NoError(t, ValidatePrivateDir(dir))
}

func TestValidatePrivateDirRejectsBroadDACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime")
	require.NoError(t, EnsurePrivateDir(dir))
	handle, err := openWindowsDir(dir)
	require.NoError(t, err)
	defer func() { _ = windows.CloseHandle(handle) }()
	userSID, err := currentWindowsUserSID()
	require.NoError(t, err)
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(t, err)
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		allowFullControl(userSID, windows.TRUSTEE_IS_USER),
		allowFullControl(world, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
	}, nil)
	require.NoError(t, err)
	require.NoError(t, windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	))

	require.Error(t, ValidatePrivateDir(dir))
}

func TestEnsurePrivateDirRejectsEmptyPath(t *testing.T) {
	require.Error(t, EnsurePrivateDir(""))
}

func TestEnsurePrivateDirRejectsSymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	link := filepath.Join(base, "link")
	require.NoError(t, os.Mkdir(target, 0o700))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	require.Error(t, EnsurePrivateDir(link))
}

func TestOpenCurrentUserFileRejectsEmptyPath(t *testing.T) {
	file, err := OpenCurrentUserFile("")
	require.Error(t, err)
	require.Nil(t, file)
}

func TestOpenCurrentUserFileAcceptsCurrentTokenOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))

	file, err := OpenCurrentUserFile(path)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}

func TestWindowsOwnerMatchesCurrentUserAndTokenOwner(t *testing.T) {
	userSID, err := currentWindowsUserSID()
	require.NoError(t, err)
	ownerSID, err := currentWindowsOwnerSID()
	require.NoError(t, err)

	require.True(t, windowsOwnerMatches(userSID, userSID, ownerSID))
	require.True(t, windowsOwnerMatches(ownerSID, userSID, ownerSID))
	require.False(t, windowsOwnerMatches(nil, userSID, ownerSID))
}

func TestCurrentUserIDIsPerUser(t *testing.T) {
	id, err := CurrentUserID()
	require.NoError(t, err)
	require.NotEmpty(t, id)
	require.NotEqual(t, "user", id)
	require.Contains(t, id, "sid-")
}

func TestCurrentWindowsOwnerSIDIsAvailable(t *testing.T) {
	ownerSID, err := currentWindowsOwnerSID()
	require.NoError(t, err)
	require.NotNil(t, ownerSID)
	require.NotEmpty(t, ownerSID.String())
}
