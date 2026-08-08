//go:build darwin || linux

package safefileio

import "os"

func restrictCurrentUserFile(
	file *os.File,
	removeACL func(*os.File) error,
) error {
	if err := ValidateCurrentUserFile(file); err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := removeACL(file); err != nil {
		return err
	}
	return file.Chmod(0o600)
}
