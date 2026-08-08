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
	return restrictCurrentUserFile(
		file,
		validateLinuxRestrictionFilesystem,
		removeLinuxAccessACLs,
	)
}

func validateLinuxRestrictionFilesystem(file *os.File) error {
	var status unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &status); err != nil {
		return fmt.Errorf("inspect file filesystem: %w", err)
	}
	if linuxFilesystemRequiresServerACL(int64(status.Type)) {
		return errors.New(
			"safefileio: current-user file restriction is unsupported " +
				"on SMB/CIFS filesystems",
		)
	}
	return nil
}

func linuxFilesystemRequiresServerACL(filesystemType int64) bool {
	switch filesystemType {
	case unix.CIFS_SUPER_MAGIC, unix.SMB_SUPER_MAGIC, unix.SMB2_SUPER_MAGIC:
		return true
	default:
		return false
	}
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
