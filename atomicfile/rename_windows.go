//go:build windows

package atomicfile

import (
	"os"

	"golang.org/x/sys/windows"

	"go.kenn.io/kit/internal/winpath"
)

// RenameNoReplace atomically renames oldpath to newpath, failing with an
// error wrapping fs.ErrExist when newpath already exists. On Windows it uses
// MoveFileEx without MOVEFILE_REPLACE_EXISTING and without
// MOVEFILE_COPY_ALLOWED, so a cross-volume move fails instead of copying.
func RenameNoReplace(oldpath, newpath string) error {
	if err := moveFileEx(oldpath, newpath, windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return &os.LinkError{Op: "MoveFileExW", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}

// replaceFile renames staging over target. MOVEFILE_COPY_ALLOWED is left out
// so a cross-volume staging directory fails instead of copying.
func replaceFile(staging, target string) error {
	err := moveFileEx(staging, target, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
	if err != nil {
		return &os.LinkError{Op: "MoveFileExW", Old: staging, New: target, Err: err}
	}
	return nil
}

func moveFileEx(from, to string, flags uint32) error {
	fromPtr, err := winpath.UTF16Ptr(from)
	if err != nil {
		return err
	}
	toPtr, err := winpath.UTF16Ptr(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(fromPtr, toPtr, flags)
}
