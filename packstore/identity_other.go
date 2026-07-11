//go:build !windows && !unix

package packstore

import (
	"io/fs"
	"os"
)

func snapshotPathIdentity(path string) (fs.FileInfo, error) { return os.Lstat(path) }

func openNoFollow(path string, durable bool) (*os.File, error) {
	flags := os.O_RDONLY
	if durable {
		flags = os.O_RDWR
	}
	return os.OpenFile(path, flags, 0)
}

func validatePlatformFileInfo(fs.FileInfo) error { return nil }
