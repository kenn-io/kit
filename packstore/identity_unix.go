//go:build unix

package packstore

import (
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

func snapshotPathIdentity(path string) (fs.FileInfo, error) { return os.Lstat(path) }

func openNoFollow(path string, durable bool) (*os.File, error) {
	flags := os.O_RDONLY | unix.O_NOFOLLOW
	if durable {
		flags = os.O_RDWR | unix.O_NOFOLLOW
	}
	return os.OpenFile(path, flags, 0)
}

func validatePlatformFileInfo(fs.FileInfo) error { return nil }
