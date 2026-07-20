//go:build !unix && !windows

package packstore

import (
	"io/fs"
	"os"
)

// Unknown platforms retain the existing no-follow readable pin and fail
// closed when the file cannot be opened safely.
func openLooseRemovalIdentityPin(path string) (*os.File, fs.FileInfo, error) {
	return openLooseRepairPin(path)
}
