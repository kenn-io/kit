//go:build linux

package packstore

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameLoosePublicationNoReplace(staging, final string) error {
	if err := unix.Renameat2(
		unix.AT_FDCWD,
		staging,
		unix.AT_FDCWD,
		final,
		unix.RENAME_NOREPLACE,
	); err != nil {
		return &os.LinkError{Op: "renameat2", Old: staging, New: final, Err: err}
	}
	return nil
}
