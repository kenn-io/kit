//go:build !unix && !windows

package safefileio

import (
	"fmt"
	"os"
	"runtime"
)

// OpenCurrentUserFile fails closed when the platform cannot bind an open file
// to a verified current-user identity without following links.
func OpenCurrentUserFile(string) (*os.File, error) {
	return nil, fmt.Errorf(
		"safefileio: current-user file opening is unsupported on %s",
		runtime.GOOS,
	)
}
