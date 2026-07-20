//go:build !unix && !windows

package packstore

import (
	"fmt"
	"runtime"
)

// replaceLooseRepairFile fails closed where Kit has no platform primitive that
// can atomically replace an existing name without destabilizing active readers.
func replaceLooseRepairFile(_, _ string) error {
	return fmt.Errorf("packstore: atomic loose repair publication is unsupported on %s", runtime.GOOS)
}
