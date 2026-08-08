package safefileio

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// RestrictCurrentUserFile validates an open handle, narrows its mode, removes
// access ACLs, and keeps it readable and writable only by its current-user owner.
func RestrictCurrentUserFile(file *os.File) error {
	return restrictCurrentUserFile(file, removeLinuxAccessACLs)
}

func removeLinuxAccessACLs(file *os.File) error {
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
	return nil
}
