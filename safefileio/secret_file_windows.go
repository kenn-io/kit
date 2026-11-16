package safefileio

import (
	"os"
)

// OpenSecretFile opens a private current-user regular file. Windows retains
// the current-user protected DACL rule and refuses final-entry reparse points.
func OpenSecretFile(path string) (*os.File, error) {
	file, err := OpenCurrentUserFile(path)
	if err != nil {
		return nil, err
	}
	err = ValidatePrivateCurrentUserFile(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
