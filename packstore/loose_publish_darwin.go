//go:build darwin

package packstore

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameLoosePublicationNoReplace(staging, final string) error {
	if err := unix.RenamexNp(staging, final, unix.RENAME_EXCL); err != nil {
		return &os.LinkError{Op: "renamex_np", Old: staging, New: final, Err: err}
	}
	return nil
}
