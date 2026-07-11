//go:build !windows && !unix

package packstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

func snapshotPathIdentity(path string) (fs.FileInfo, error) { return os.Lstat(path) }

func openNoFollow(path string, durable bool) (*os.File, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := validateRegularNoFollow(path, pathInfo); err != nil {
		return nil, err
	}
	flags := os.O_RDONLY
	if durable {
		flags = os.O_RDWR
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, err
	}
	descriptorInfo, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if err := validateRegularNoFollow(path, descriptorInfo); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if !os.SameFile(pathInfo, descriptorInfo) {
		return nil, errors.Join(fmt.Errorf("%w: %w", ErrContentMismatch, errIdentityChanged), f.Close())
	}
	return f, nil
}

func validatePlatformFileInfo(fs.FileInfo) error { return nil }
