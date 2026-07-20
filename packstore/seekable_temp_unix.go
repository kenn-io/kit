//go:build unix

package packstore

import (
	"errors"
	"fmt"
	"os"
)

// createSeekableLooseTempPlatform removes the only pathname while retaining
// the open descriptor. Closing the descriptor therefore releases the exact
// temporary inode without consulting a pathname that another process can
// replace. removeLoosePathPinned makes the initial unlink race-safe too.
func createSeekableLooseTempPlatform() (*os.File, error) {
	file, err := os.CreateTemp("", "packstore-loose-open-")
	if err != nil {
		return nil, err
	}
	if _, err := removeLoosePathPinned(file.Name(), file); err != nil {
		return nil, errors.Join(
			fmt.Errorf("packstore: unlink seekable loose temporary file: %w", err),
			file.Close(),
		)
	}
	return file, nil
}
