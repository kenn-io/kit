//go:build unix && !darwin && !linux

package safefileio

import (
	"fmt"
	"os"
	"runtime"
)

// RestrictCurrentUserFile fails closed on Unix platforms where Kit cannot
// remove access-control lists through the verified file handle.
func RestrictCurrentUserFile(file *os.File) error {
	if err := ValidateCurrentUserFile(file); err != nil {
		return err
	}
	return fmt.Errorf(
		"safefileio: current-user file restriction is unsupported on %s",
		runtime.GOOS,
	)
}
