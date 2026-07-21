//go:build !darwin && !linux && !windows

package packstore

import (
	"errors"
	"os"
)

// Platforms without an atomic no-replace rename retain hard-link publication
// and fail closed when the filesystem rejects it.
func renameLoosePublicationNoReplace(staging, final string) error {
	return &os.LinkError{Op: "rename-noreplace", Old: staging, New: final, Err: errors.ErrUnsupported}
}
