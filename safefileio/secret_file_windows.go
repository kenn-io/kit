package safefileio

import (
	"context"
	"os"
)

// OpenSecretFile opens a private current-user regular file. Windows retains
// the current-user protected DACL rule and refuses final-entry reparse points.
func OpenSecretFile(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := OpenCurrentUserFile(path)
	if err != nil {
		return nil, err
	}
	err = ValidatePrivateCurrentUserFile(file)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
