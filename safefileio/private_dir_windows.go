//go:build windows

package safefileio

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// EnsurePrivateDir creates path when needed and verifies it is a non-reparse
// directory owned by the current token user or token owner with a
// user/system/admin-only DACL.
func EnsurePrivateDir(path string) error {
	if path == "" {
		return fmt.Errorf("path is empty")
	}
	if err := rejectWindowsReparsePoint(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := rejectWindowsReparsePoint(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	handle, err := openWindowsDir(path)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	userSID, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	ownerSID, err := currentWindowsOwnerSID()
	if err != nil {
		return err
	}
	if err := verifyWindowsDirHandle(path, handle, userSID, ownerSID); err != nil {
		return err
	}
	return restrictWindowsDir(handle, userSID)
}

// ValidatePrivateDir verifies path is a non-reparse directory owned by the
// current token user or token owner. It never creates or changes the directory.
func ValidatePrivateDir(path string) error {
	if path == "" {
		return fmt.Errorf("path is empty")
	}
	if err := rejectWindowsReparsePoint(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	handle, err := openWindowsDir(path)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	userSID, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	ownerSID, err := currentWindowsOwnerSID()
	if err != nil {
		return err
	}
	if err := verifyWindowsDirHandle(path, handle, userSID, ownerSID); err != nil {
		return err
	}
	return verifyWindowsDirDACL(path, handle, userSID, ownerSID)
}

// CurrentUserID returns a stable filesystem-safe identifier for the current
// Windows account.
func CurrentUserID() (string, error) {
	sid, err := currentWindowsUserSID()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(sid.String()))
	return "sid-" + hex.EncodeToString(sum[:8]), nil
}

func rejectWindowsReparsePoint(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(path16)
	if err != nil {
		return err
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s is a reparse point", path)
	}
	return nil
}

func openWindowsDir(path string) (windows.Handle, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(
		path16,
		windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
}

func verifyWindowsDirHandle(path string, handle windows.Handle, userSID, ownerSID *windows.SID) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s is a reparse point", path)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("%s is not a directory", path)
	}
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	if owner == nil {
		return fmt.Errorf("%s owner is missing", path)
	}
	if !windowsOwnerMatches(owner, userSID, ownerSID) {
		return fmt.Errorf("%s is not owned by current user or token owner", path)
	}
	return nil
}

func verifyWindowsDirDACL(path string, handle windows.Handle, userSID, ownerSID *windows.SID) error {
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("%s DACL is not protected", path)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return fmt.Errorf("%s DACL is empty", path)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	allowed := []*windows.SID{userSID, ownerSID, system, admins}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("%s DACL contains non-allow ACE", path)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !windowsAnyOwnerMatches(sid, allowed) {
			return fmt.Errorf("%s DACL grants access to unexpected principal", path)
		}
	}
	return nil
}

func windowsAnyOwnerMatches(owner *windows.SID, allowed []*windows.SID) bool {
	for _, sid := range allowed {
		if sid != nil && owner != nil && owner.Equals(sid) {
			return true
		}
	}
	return false
}

func restrictWindowsDir(handle windows.Handle, userSID *windows.SID) error {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	entries := []windows.EXPLICIT_ACCESS{
		allowFullControl(userSID, windows.TRUSTEE_IS_USER),
		allowFullControl(system, windows.TRUSTEE_IS_USER),
		allowFullControl(admins, windows.TRUSTEE_IS_GROUP),
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	)
}

func allowFullControl(sid *windows.SID, trusteeType windows.TRUSTEE_TYPE) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  trusteeType,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}
