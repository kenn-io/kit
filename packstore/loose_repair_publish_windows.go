//go:build windows

package packstore

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procReplaceFileW           = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")
	replaceLooseFileWindows    = replaceLooseFileWindowsAPI
	linkLooseRepairFileWindows = os.Link
)

// replaceLooseRepairFile uses ReplaceFileW when the canonical name exists.
// ReplaceFileW preserves handles opened with FILE_SHARE_DELETE, as Kit's
// no-follow reader does. If the target is absent, a hard link creates it
// atomically without clobbering a name planted by a concurrent process; losing
// that creation race returns to ReplaceFileW rather than remove-then-rename.
// The absent-target link may leave the staging name for caller-owned cleanup.
func replaceLooseRepairFile(staging, final string, verified fs.FileInfo) (looseRepairPublishResult, error) {
	backup, err := newLooseRepairBackupPath(final)
	if err != nil {
		return looseRepairPublishResult{KeepStaging: true, SyncStaging: true}, err
	}
	err = replaceLooseFileWindows(staging, final, backup)
	if err == nil {
		return looseRepairPublishResult{Created: true, SyncShard: true}, cleanupLooseRepairBackup(backup, true)
	}
	if !isWindowsNotExist(err) {
		return reconcileLooseRepairReplacement(staging, final, backup, verified, err)
	}
	if err := linkLooseRepairFileWindows(staging, final); err == nil {
		return looseRepairPublishResult{Created: true, SyncShard: true}, nil
	} else if !errors.Is(err, fs.ErrExist) {
		return reconcileLooseRepairReplacement(staging, final, backup, verified, err)
	}
	err = replaceLooseFileWindows(staging, final, backup)
	if err == nil {
		return looseRepairPublishResult{Created: true, SyncShard: true}, cleanupLooseRepairBackup(backup, true)
	}
	return reconcileLooseRepairReplacement(staging, final, backup, verified, err)
}

func isWindowsNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func replaceLooseFileWindowsAPI(staging, final, backup string) error {
	replacedName, err := windows.UTF16PtrFromString(final)
	if err != nil {
		return err
	}
	replacementName, err := windows.UTF16PtrFromString(staging)
	if err != nil {
		return err
	}
	backupName, err := windows.UTF16PtrFromString(backup)
	if err != nil {
		return err
	}
	r1, _, callErr := procReplaceFileW.Call(
		uintptr(unsafe.Pointer(replacedName)),
		uintptr(unsafe.Pointer(replacementName)),
		uintptr(unsafe.Pointer(backupName)),
		0,
		0,
		0,
	)
	if r1 != 0 {
		return nil
	}
	if errors.Is(callErr, syscall.Errno(0)) {
		callErr = syscall.EINVAL
	}
	return &os.LinkError{Op: "ReplaceFileW", Old: staging, New: final, Err: callErr}
}
