//go:build !windows

package atomicfile

import "os"

// replaceFile renames staging over target. rename(2) never copies, so a
// cross-device staging directory fails with EXDEV.
func replaceFile(staging, target string) error {
	return os.Rename(staging, target)
}
