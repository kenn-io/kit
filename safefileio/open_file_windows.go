//go:build windows

package safefileio

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// OpenCurrentUserFile opens path without following reparse points and verifies
// the opened handle is a regular file owned by the current user.
func OpenCurrentUserFile(path string) (*os.File, error) {
	if path == "" {
		return nil, fmt.Errorf("path is empty")
	}
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		path16,
		windows.GENERIC_READ|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = windows.CloseHandle(handle)
		}
	}()
	if err := validateWindowsFileHandle(path, handle); err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	success = true
	return file, nil
}

func validateWindowsFileHandle(path string, handle windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s is a reparse point", path)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return fmt.Errorf("%s is a directory", path)
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
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if !owner.Equals(user.User.Sid) {
		return fmt.Errorf("%s is not owned by current user", path)
	}
	return nil
}
