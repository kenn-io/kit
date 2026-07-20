//go:build unix

package packstore

import (
	"io/fs"
	"os"
)

// replaceLooseRepairFile atomically replaces an existing canonical name or
// creates an absent one. POSIX rename keeps open descriptors pinned to the old
// inode and never exposes a remove-then-rename gap.
func replaceLooseRepairFile(staging, final string, _ fs.FileInfo) (looseRepairPublishResult, error) {
	if err := os.Rename(staging, final); err != nil {
		return looseRepairPublishResult{}, err
	}
	return looseRepairPublishResult{Created: true, SyncShard: true}, nil
}
