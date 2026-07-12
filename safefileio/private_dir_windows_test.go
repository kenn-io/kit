//go:build windows

package safefileio

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
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
	require := require.New(t)
	assert := assert.New(t)
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
	require := require.New(t)
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
			assert := assert.New(t)
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
