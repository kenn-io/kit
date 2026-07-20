//go:build unix

package packstore

import "os"

// replaceLooseRepairFile atomically replaces an existing canonical name or
// creates an absent one. POSIX rename keeps open descriptors pinned to the old
// inode and never exposes a remove-then-rename gap.
func replaceLooseRepairFile(staging, final string) error {
	return os.Rename(staging, final)
}
