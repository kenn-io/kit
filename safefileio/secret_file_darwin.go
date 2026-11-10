package safefileio

import (
	"os"

	"golang.org/x/sys/unix"
)

func validateSecretAccess(file *os.File) error { return validateDarwinExtendedACL(file) }
func validateSecretParentAccess(dir *os.File) error {
	if err := rejectRemoteParent(dir); err != nil {
		return err
	}
	return validateDarwinACL(dir, true)
}

func openSecretDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_SEARCH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
