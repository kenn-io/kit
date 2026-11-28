//go:build windows

package safefileio

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

var gcChurnSink atomic.Pointer[[]byte]

func startGCChurn(t *testing.T) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() {
		for ctx.Err() == nil {
			// Allocate and fill 50-byte slices (the 64-byte size class used by
			// CreateWellKnownSid) with 0xff so freed unpinned SIDs are overwritten.
			for range 64 {
				poison := bytes.Repeat([]byte{0xff}, 50)
				gcChurnSink.Store(&poison)
			}
			runtime.GC()
		}
	})
	return func() {
		cancel()
		wg.Wait()
	}
}

func TestPrivateWindowsACLDeduplicatesLocalSystem(t *testing.T) {
	require := require.New(t)
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	require.NoError(err)
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	require.NoError(err)
	acl, err := privateWindowsACL(system, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
	require.NoError(err)
	require.Equal(uint16(2), acl.AceCount)
	principals := make(map[string]bool)
	for i := range acl.AceCount {
		var ace *windows.ACCESS_ALLOWED_ACE
		require.NoError(windows.GetAce(acl, uint32(i), &ace))
		require.NotNil(ace)
		principals[(*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()] = true
	}
	assert.Equal(t, map[string]bool{system.String(): true, admins.String(): true}, principals)
}

func TestEnsurePrivateDirCreatesOwnedDirectory(t *testing.T) {
	require := require.New(t)
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
	require.NoError(t, EnsurePrivateDir(dir))

	require.NoError(t, ValidatePrivateDir(dir))
}

func TestEnsurePrivateDirShallowLeavesMatchingDACLUntouched(t *testing.T) {
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), "runtime")
	require.NoError(EnsurePrivateDir(dir))
	userSID, err := currentWindowsUserSID()
	require.NoError(err)
	wanted, err := privateWindowsACL(userSID, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
	require.NoError(err)
	parent, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(err)
	current, _, err := parent.DACL()
	require.NoError(err)
	require.Equal(windowsACLBytes(wanted), windowsACLBytes(current))
	source := filepath.Join(t.TempDir(), "source")
	require.NoError(EnsurePrivateDir(source))
	sourceHandle, err := openWindowsDir(source)
	require.NoError(err)
	defer func() { _ = windows.CloseHandle(sourceHandle) }()
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(err)
	var pinner runtime.Pinner
	defer pinner.Unpin()
	worldRead := allowFullControl(&pinner, world, windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
	worldRead.AccessPermissions = windows.FILE_GENERIC_READ
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		allowFullControl(&pinner, userSID, windows.TRUSTEE_IS_USER, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
		worldRead,
	}, nil)
	require.NoError(err)
	require.NoError(restrictWindowsDir(sourceHandle, acl))
	original := filepath.Join(source, "record")
	require.NoError(os.WriteFile(original, nil, 0o600))
	path := filepath.Join(dir, "record")
	// A move on the same volume preserves the child's original inherited ACL.
	require.NoError(os.Rename(original, path))
	before, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(err)
	control, _, err := before.Control()
	require.NoError(err)
	require.Zero(control & windows.SE_DACL_PROTECTED)
	outsiderAccess := func(descriptor *windows.SECURITY_DESCRIPTOR) uint32 {
		dacl, _, err := descriptor.DACL()
		require.NoError(err)
		require.NotNil(dacl)
		var access uint32
		for i := range dacl.AceCount {
			var ace *windows.ACCESS_ALLOWED_ACE
			require.NoError(windows.GetAce(dacl, uint32(i), &ace))
			require.NotNil(ace)
			if ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE && world.Equals((*windows.SID)(unsafe.Pointer(&ace.SidStart))) {
				require.NotZero(ace.Header.AceFlags & windows.INHERITED_ACE)
				access |= uint32(ace.Mask)
			}
		}
		return access
	}
	require.Equal(uint32(windows.FILE_GENERIC_READ), outsiderAccess(before))

	require.NoError(EnsurePrivateDirShallow(dir))
	after, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(err)
	assert.Equal(t, before.String(), after.String())

	require.NoError(EnsurePrivateDir(dir))
	propagated, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(err)
	assert.Zero(t, outsiderAccess(propagated))
}

func TestEnsurePrivateDirShallowRewritesDifferentPrivateDACL(t *testing.T) {
	userSID, err := currentWindowsUserSID()
	require.NoError(t, err)
	wanted, err := privateWindowsACL(userSID, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
	require.NoError(t, err)

	tests := []struct {
		name        string
		sddl        string
		unprotected bool
	}{
		{name: "read-only children", sddl: "D:P(A;;GA;;;" + userSID.String() + ")(A;OIIO;GR;;;" + userSID.String() + ")"},
		{name: "generic full control", sddl: "D:P(A;OICI;GA;;;" + userSID.String() + ")(A;OICI;GA;;;SY)(A;OICI;GA;;;BA)"},
		{name: "unprotected matching", sddl: "D:(A;OICI;FA;;;" + userSID.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", unprotected: true},
		{name: "null DACL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			dir := filepath.Join(t.TempDir(), "runtime")
			parentHandle, err := openWindowsDir(filepath.Dir(dir))
			require.NoError(err)
			defer func() { _ = windows.CloseHandle(parentHandle) }()
			parentACL, err := privateWindowsACL(userSID, windows.NO_INHERITANCE)
			require.NoError(err)
			require.NoError(restrictWindowsDir(parentHandle, parentACL))
			require.NoError(os.Mkdir(dir, 0o700))
			handle, err := openWindowsDir(dir)
			require.NoError(err)
			defer func() { _ = windows.CloseHandle(handle) }()
			var acl *windows.ACL
			if tt.sddl != "" {
				descriptor, err := windows.SecurityDescriptorFromString(tt.sddl)
				require.NoError(err)
				acl, _, err = descriptor.DACL()
				require.NoError(err)
			}
			if tt.unprotected {
				require.NoError(windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
				require.Error(ValidatePrivateDir(dir))
			} else if acl == nil {
				require.NoError(windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, nil, nil))
				require.Error(ValidatePrivateDir(dir))
			} else {
				require.NoError(restrictWindowsDir(handle, acl))
				require.NoError(ValidatePrivateDir(dir))
			}
			before, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
			require.NoError(err)
			current, _, err := before.DACL()
			if acl == nil {
				if err != nil {
					require.ErrorIs(err, windows.ERROR_OBJECT_NOT_FOUND)
				}
				require.Nil(current)
			} else if tt.unprotected {
				require.NoError(err)
				require.NotNil(current)
				control, _, err := before.Control()
				require.NoError(err)
				require.Zero(control & windows.SE_DACL_PROTECTED)
				require.Equal(windowsACLBytes(wanted), windowsACLBytes(current))
			} else {
				require.NoError(err)
				require.NotNil(current)
				require.NotEqual(windowsACLBytes(wanted), windowsACLBytes(current))
			}

			require.NoError(EnsurePrivateDirShallow(dir))
			after, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
			require.NoError(err)
			control, _, err := after.Control()
			require.NoError(err)
			assert.NotZero(t, control&windows.SE_DACL_PROTECTED)
			current, _, err = after.DACL()
			require.NoError(err)
			require.NotNil(current)
			assert.Equal(t, windowsACLBytes(wanted), windowsACLBytes(current))
		})
	}
}

func TestValidatePrivateDirRejectsBroadDACL(t *testing.T) {
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), "runtime")
	require.NoError(EnsurePrivateDir(dir))
	handle, err := openWindowsDir(dir)
	require.NoError(err)
	defer func() { _ = windows.CloseHandle(handle) }()
	userSID, err := currentWindowsUserSID()
	require.NoError(err)
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(err)
	var pinner runtime.Pinner
	defer pinner.Unpin()
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		allowFullControl(&pinner, userSID, windows.TRUSTEE_IS_USER, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
		allowFullControl(&pinner, world, windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
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

func TestValidatePrivateCurrentUserFileRejectsBroadDACL(t *testing.T) {
	require := require.New(t)
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
	var pinner runtime.Pinner
	defer pinner.Unpin()
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		allowFullControl(&pinner, userSID, windows.TRUSTEE_IS_USER, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
		allowFullControl(&pinner, world, windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
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
	require.Error(ValidateTrustedFile(file))
	require.Error(verifyWindowsDirDACL(path, handle, userSID, ownerSID))
}

func TestValidateCurrentUserFileProtectionRequirements(t *testing.T) {
	require := require.New(t)
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
	var pinner runtime.Pinner
	defer pinner.Unpin()
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		allowFullControl(&pinner, userSID, windows.TRUSTEE_IS_USER, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
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
	require.NoError(ValidateTrustedFile(file))
}

func TestValidatePrivateCurrentUserFileAcceptsProtectedPrivateDACL(t *testing.T) {
	require := require.New(t)
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
	acl, err := privateWindowsACL(userSID, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
	require.NoError(err)
	require.NoError(restrictWindowsDir(handle, acl))

	require.NoError(ValidatePrivateCurrentUserFile(file))
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
	require := require.New(t)
	id, err := CurrentUserID()
	require.NoError(err)
	require.NotEmpty(id)
	require.NotEqual("user", id)
	require.Contains(id, "sid-")
}

func TestCurrentWindowsOwnerSIDIsAvailable(t *testing.T) {
	ownerSID, err := currentWindowsOwnerSID()
	require.NoError(t, err)
	require.NotNil(t, ownerSID)
	require.NotEmpty(t, ownerSID.String())
}

func TestCurrentWindowsUserSIDIsAvailable(t *testing.T) {
	require := require.New(t)
	userSID, err := currentWindowsUserSID()
	require.NoError(err)
	require.NotNil(userSID)
	require.True(userSID.IsValid())

	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(err)
	require.True(userSID.Equals(tokenUser.User.Sid))
}

func TestPrivateWindowsACLUnderGCPressure(t *testing.T) {
	stopChurn := startGCChurn(t)
	defer stopChurn()

	const workers = 4
	base := t.TempDir()
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for worker := range workers {
		dir := filepath.Join(base, fmt.Sprintf("worker-%d", worker))
		wg.Go(func() {
			for i := range 128 {
				userSID, err := currentWindowsUserSID()
				if err != nil {
					errs[worker] = fmt.Errorf("iteration %d user SID: %w", i, err)
					return
				}
				acl, err := privateWindowsACL(userSID, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
				if err != nil {
					errs[worker] = fmt.Errorf("iteration %d privateWindowsACL: %w", i, err)
					return
				}
				if acl == nil || acl.AceCount < 2 {
					errs[worker] = fmt.Errorf("iteration %d unexpected ACL: %v", i, acl)
					return
				}
			}
			for i := range 8 {
				if err := EnsurePrivateDirShallow(dir); err != nil {
					errs[worker] = fmt.Errorf("iteration %d EnsurePrivateDirShallow: %w", i, err)
					return
				}
			}
		})
	}
	wg.Wait()
	for worker, err := range errs {
		require.NoError(t, err, "worker %d", worker)
	}
}
