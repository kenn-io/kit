package safefileio

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// RestrictCurrentUserFile validates an open handle, removes access ACLs, and
// makes the file readable and writable only by its current-user owner.
func RestrictCurrentUserFile(file *os.File) error {
	if err := ValidateCurrentUserFile(file); err != nil {
		return err
	}
	for _, attribute := range []string{
		"system.posix_acl_access",
		"system.nfs4_acl",
	} {
		err := unix.Fremovexattr(int(file.Fd()), attribute)
		if err != nil && !errors.Is(err, unix.ENODATA) &&
			!errors.Is(err, unix.ENOTSUP) {
			return fmt.Errorf("remove access ACL %s: %w", attribute, err)
		}
	}
	return file.Chmod(0o600)
}
