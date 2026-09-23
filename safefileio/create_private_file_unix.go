//go:build unix

package safefileio

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// CreatePrivateFile creates a new regular file at path, opened read-write,
// that only the current user can access from the moment it exists. It fails
// with an error wrapping fs.ErrExist when anything, including a dangling
// symlink, already exists at path. It never repairs an existing file.
func CreatePrivateFile(path string) (*os.File, error) {
	if path == "" {
		return nil, errors.New("path is empty")
	}
	file, err := os.OpenFile(
		path,
		os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC,
		0o600,
	)
	if err != nil {
		return nil, err
	}
	if err := securePrivateFile(file); err != nil {
		return nil, errors.Join(err, discardCreatedFile(path, file))
	}
	return file, nil
}

func securePrivateFile(file *os.File) error {
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	// ValidatePrivateCurrentUserFile also checks type and ownership, and fails
	// closed on Unix platforms where private access cannot be verified.
	return ValidatePrivateCurrentUserFile(file)
}

// discardCreatedFile closes file and removes path only while path still names
// the file this package created, so a replaced entry is never deleted.
func discardCreatedFile(path string, file *os.File) error {
	created, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return errors.Join(fmt.Errorf("stat created file %s: %w", path, statErr), closeErr)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return errors.Join(fmt.Errorf("lstat created file %s: %w", path, err), closeErr)
	}
	if !os.SameFile(created, current) {
		return errors.Join(fmt.Errorf("%s was replaced after creation; not removing it", path), closeErr)
	}
	if err := os.Remove(path); err != nil {
		return errors.Join(fmt.Errorf("remove created file %s: %w", path, err), closeErr)
	}
	return closeErr
}
