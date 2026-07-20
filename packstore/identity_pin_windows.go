//go:build windows

package packstore

import (
	"io/fs"
	"os"
)

// Windows already supplies a zero-access, reparse-point-safe identity handle
// for loose repair. Reuse it for removal claims without requiring file reads.
func openLooseRemovalIdentityPin(path string) (*os.File, fs.FileInfo, error) {
	return openLooseRepairPin(path)
}
