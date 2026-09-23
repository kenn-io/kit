//go:build unix

package safefileio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// the file this package created, so a replaced entry is never deleted. Unix
// cannot unlink by handle, so the identity check and the removal are separate
// steps. Removal therefore also requires a parent directory that other users
// cannot rename entries in; otherwise the empty private file is left in place.
func discardCreatedFile(path string, file *os.File) error {
	created, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return errors.Join(fmt.Errorf("stat created file %s: %w", path, statErr), closeErr)
	}
	if err := requireUnsharedParent(filepath.Dir(path)); err != nil {
		return errors.Join(fmt.Errorf("leaving created file %s in place: %w", path, err), closeErr)
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

// requireUnsharedParent reports an error unless only the current user or root
// can rename entries in dir: dir must be owned by one of them and either deny
// group and other write access or have the sticky bit set.
func requireUnsharedParent(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("parent %s is not a directory", dir)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %s: missing owner information", dir)
	}
	if stat.Uid != uint32(os.Getuid()) && stat.Uid != 0 {
		return fmt.Errorf("parent %s is owned by another user", dir)
	}
	if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("other users can rename entries in parent %s", dir)
	}
	return nil
}
