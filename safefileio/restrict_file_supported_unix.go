//go:build darwin || linux

package safefileio

import (
	"fmt"
	"os"
)

func restrictCurrentUserFile(
	file *os.File,
	preflight func(*os.File) error,
	removeACL func(*os.File) error,
) error {
	if err := ValidateCurrentUserFile(file); err != nil {
		return err
	}
	if preflight != nil {
		if err := preflight(file); err != nil {
			return err
		}
	}
	if err := setPrivateFileMode(file); err != nil {
		return err
	}
	if err := removeACL(file); err != nil {
		return err
	}
	return setPrivateFileMode(file)
}

func setPrivateFileMode(file *os.File) error {
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	return verifyPrivateFileMode(file)
}

func verifyPrivateFileMode(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		return fmt.Errorf("safefileio: file mode is %04o, not 0600", mode)
	}
	return nil
}
