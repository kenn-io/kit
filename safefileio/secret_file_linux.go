package safefileio

import (
	"os"

	"golang.org/x/sys/unix"
)

func validateSecretAccess(file *os.File) error      { return validateLinuxPrivateAccess(file) }
func validateSecretParentAccess(dir *os.File) error { return verifyParentAccessPolicy(dir) }

func openSecretDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
