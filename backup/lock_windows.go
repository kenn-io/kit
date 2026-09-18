//go:build windows

package backup

import (
	"errors"

	"golang.org/x/sys/windows"
)

// lockFileBusy reports whether err means another handle holds the lock file
// open. Go opens files without FILE_SHARE_DELETE, so a rename fails for as
// long as any reader, such as a waiting locker describing the holder, has the
// file open.
func lockFileBusy(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
