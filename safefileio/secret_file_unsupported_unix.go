//go:build unix && !darwin && !linux

package safefileio

import (
	"os"
	"syscall"
)

func validateSecretAccess(*os.File) error       { return privateFileCreationSupported() }
func validateSecretParentAccess(*os.File) error { return privateFileCreationSupported() }

func openSecretDirectory(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
