//go:build windows

package packstore

import (
	"os"

	"golang.org/x/sys/windows"
)

func renameLoosePublicationNoReplace(staging, final string) error {
	stagingName, err := windows.UTF16PtrFromString(staging)
	if err != nil {
		return err
	}
	finalName, err := windows.UTF16PtrFromString(final)
	if err != nil {
		return err
	}
	if err := windows.MoveFile(stagingName, finalName); err != nil {
		return &os.LinkError{Op: "MoveFileW", Old: staging, New: final, Err: err}
	}
	return nil
}
