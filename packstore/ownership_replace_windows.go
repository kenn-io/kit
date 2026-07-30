//go:build windows

package packstore

import "golang.org/x/sys/windows"

func replaceOwnershipFile(staged, target string) error {
	staged16, err := windows.UTF16PtrFromString(staged)
	if err != nil {
		return err
	}
	target16, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		staged16,
		target16,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}
