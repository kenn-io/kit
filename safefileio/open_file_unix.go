//go:build !windows

package safefileio

import (
	"fmt"
	"os"
	"syscall"
)

// OpenCurrentUserFile opens path without following symlinks and verifies the
// opened handle is a regular file owned by the current user.
func OpenCurrentUserFile(path string) (*os.File, error) {
	if path == "" {
		return nil, fmt.Errorf("path is empty")
	}
	file, err := os.OpenFile(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("stat %s: missing owner information", path)
	}
	if stat.Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("%s is not owned by current user", path)
	}
	success = true
	return file, nil
}
