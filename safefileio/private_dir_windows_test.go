//go:build windows

package safefileio

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestEnsurePrivateDirCreatesOwnedDirectory(t *testing.T) {
	require := Require.New(t)
	dir := filepath.Join(t.TempDir(), "runtime")

	require.NoError(EnsurePrivateDir(dir))

	ownerSID, err := currentWindowsOwnerSID()
	require.NoError(err)
	descriptor, err := windows.GetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	require.NoError(err)
	owner, _, err := descriptor.Owner()
	require.NoError(err)
	require.NotNil(owner)
	require.True(owner.Equals(ownerSID))
}

func TestValidatePrivateDirAcceptsPrivateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime")
	Require.NoError(t, EnsurePrivateDir(dir))

	Require.NoError(t, ValidatePrivateDir(dir))
}

func TestValidatePrivateDirRejectsBroadDACL(t *testing.T) {
	require := Require.New(t)
	dir := filepath.Join(t.TempDir(), "runtime")
	require.NoError(EnsurePrivateDir(dir))
	handle, err := openWindowsDir(dir)
	require.NoError(err)
	defer func() { _ = windows.CloseHandle(handle) }()
	userSID, err := currentWindowsUserSID()
	require.NoError(err)
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(err)
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		allowFullControl(userSID, windows.TRUSTEE_IS_USER),
		allowFullControl(world, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
	}, nil)
	require.NoError(err)
	require.NoError(windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	))

	require.Error(ValidatePrivateDir(dir))
}

func TestEnsurePrivateDirRejectsEmptyPath(t *testing.T) {
	Require.Error(t, EnsurePrivateDir(""))
}

func TestEnsurePrivateDirRejectsSymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	link := filepath.Join(base, "link")
	Require.NoError(t, os.Mkdir(target, 0o700))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	Require.Error(t, EnsurePrivateDir(link))
}

func TestOpenCurrentUserFileRejectsEmptyPath(t *testing.T) {
	file, err := OpenCurrentUserFile("")
	Require.Error(t, err)
	Require.Nil(t, file)
}

func TestOpenCurrentUserFileAcceptsCurrentTokenOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	Require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))

	file, err := OpenCurrentUserFile(path)
	Require.NoError(t, err)
	Require.NoError(t, file.Close())
}

func TestValidatePrivateCurrentUserFileRejectsBroadDACL(t *testing.T) {
	require := Require.New(t)
	path := filepath.Join(t.TempDir(), "record.json")
	require.NoError(os.WriteFile(path, []byte("{}"), 0o600))
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	defer func() { _ = file.Close() }()
	path16, err := windows.UTF16PtrFromString(path)
	require.NoError(err)
	handle, err := windows.CreateFile(
		path16,
		windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	require.NoError(err)
	defer func() { _ = windows.CloseHandle(handle) }()
	userSID, err := currentWindowsUserSID()
	require.NoError(err)
	ownerSID, err := currentWindowsOwnerSID()
	require.NoError(err)
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(err)
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		allowFullControl(userSID, windows.TRUSTEE_IS_USER),
		allowFullControl(world, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
	}, nil)
	require.NoError(err)
	require.NoError(windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	))
	require.Error(verifyWindowsDirDACL(path, handle, userSID, ownerSID))

	require.Error(ValidatePrivateCurrentUserFile(file))
	require.Error(verifyWindowsDirDACL(path, handle, userSID, ownerSID))
}

func TestValidatePrivateCurrentUserFileRejectsUnprotectedPrivateDACL(t *testing.T) {
	require := Require.New(t)
	dir := filepath.Join(t.TempDir(), "private")
	require.NoError(EnsurePrivateDir(dir))
	path := filepath.Join(dir, "record.json")
	require.NoError(os.WriteFile(path, []byte("{}"), 0o600))
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	defer func() { _ = file.Close() }()
	path16, err := windows.UTF16PtrFromString(path)
	require.NoError(err)
	handle, err := windows.CreateFile(
		path16,
		windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	require.NoError(err)
	defer func() { _ = windows.CloseHandle(handle) }()
	userSID, err := currentWindowsUserSID()
	require.NoError(err)
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		allowFullControl(userSID, windows.TRUSTEE_IS_USER),
	}, nil)
	require.NoError(err)
	require.NoError(windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	))

	require.Error(ValidatePrivateCurrentUserFile(file))
}

func TestValidatePrivateCurrentUserFileAcceptsProtectedPrivateDACL(t *testing.T) {
	require := Require.New(t)
	dir := filepath.Join(t.TempDir(), "private")
	require.NoError(EnsurePrivateDir(dir))
	path := filepath.Join(dir, "record.json")
	require.NoError(os.WriteFile(path, []byte("{}"), 0o600))
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	defer func() { _ = file.Close() }()
	path16, err := windows.UTF16PtrFromString(path)
	require.NoError(err)
	handle, err := windows.CreateFile(
		path16,
		windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	require.NoError(err)
	defer func() { _ = windows.CloseHandle(handle) }()
	userSID, err := currentWindowsUserSID()
	require.NoError(err)
	require.NoError(restrictWindowsDir(handle, userSID))

	require.NoError(ValidatePrivateCurrentUserFile(file))
}

func TestWindowsOwnerMatchesCurrentUserAndTokenOwner(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)
	userSID, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	require.NoError(err)
	ownerSID, err := windows.CreateWellKnownSid(windows.WinBuiltinGuestsSid)
	require.NoError(err)
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	require.NoError(err)
	adminsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	require.NoError(err)

	assert.True(windowsOwnerMatches(userSID, userSID, ownerSID))
	assert.True(windowsOwnerMatches(ownerSID, userSID, ownerSID))
	assert.False(windowsOwnerMatches(systemSID, userSID, ownerSID))
	assert.False(windowsOwnerMatches(adminsSID, userSID, ownerSID))
	assert.False(windowsOwnerMatches(nil, userSID, ownerSID))
}

func TestVerifyWindowsDirectoryOwner(t *testing.T) {
	require := Require.New(t)
	userSID, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	require.NoError(err)
	ownerSID, err := windows.CreateWellKnownSid(windows.WinBuiltinGuestsSid)
	require.NoError(err)
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	require.NoError(err)
	adminsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	require.NoError(err)
	worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(err)

	tests := []struct {
		name      string
		owner     *windows.SID
		wantError string
	}{
		{name: "current user", owner: userSID},
		{name: "token owner", owner: ownerSID},
		{name: "LocalSystem", owner: systemSID},
		{name: "Administrators", owner: adminsSID},
		{
			name:      "World",
			owner:     worldSID,
			wantError: "runtime is not owned by current user, token owner, LocalSystem, or built-in Administrators",
		},
		{
			name:      "missing",
			owner:     nil,
			wantError: "runtime is not owned by current user, token owner, LocalSystem, or built-in Administrators",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := Assert.New(t)
			err := verifyWindowsDirectoryOwner("runtime", tt.owner, userSID, ownerSID)
			if tt.wantError != "" {
				assert.EqualError(err, tt.wantError)
			} else {
				assert.NoError(err)
			}
		})
	}
}

func TestCurrentUserIDIsPerUser(t *testing.T) {
	require := Require.New(t)
	id, err := CurrentUserID()
	require.NoError(err)
	require.NotEmpty(id)
	require.NotEqual("user", id)
	require.Contains(id, "sid-")
}

func TestCurrentWindowsOwnerSIDIsAvailable(t *testing.T) {
	ownerSID, err := currentWindowsOwnerSID()
	Require.NoError(t, err)
	Require.NotNil(t, ownerSID)
	Require.NotEmpty(t, ownerSID.String())
}
