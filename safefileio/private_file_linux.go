package safefileio

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ValidatePrivateCurrentUserFile verifies that an open current-user-owned file
// has private mode bits and no access ACL that could grant another principal.
func ValidatePrivateCurrentUserFile(file *os.File) error {
	return validatePrivateCurrentUserFile(file, validateLinuxPrivateAccess)
}

func validateLinuxPrivateAccess(file *os.File) error {
	var status unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &status); err != nil {
		return fmt.Errorf("inspect file filesystem: %w", err)
	}
	if linuxFilesystemRequiresServerACL(int64(status.Type)) {
		return errors.New(
			"safefileio: private current-user file validation is unsupported " +
				"on SMB/CIFS filesystems",
		)
	}
	return validateLinuxAccessACLs(file)
}

func linuxFilesystemRequiresServerACL(filesystemType int64) bool {
	switch uint32(filesystemType) {
	case uint32(unix.CIFS_SUPER_MAGIC),
		uint32(unix.SMB_SUPER_MAGIC),
		uint32(unix.SMB2_SUPER_MAGIC):
		return true
	default:
		return false
	}
}

func validateLinuxAccessACLs(file *os.File) error {
	for _, attribute := range []string{
		"system.posix_acl_access",
		"system.nfs4_acl",
		"system.cifs_acl",
	} {
		_, err := unix.Fgetxattr(int(file.Fd()), attribute, nil)
		if err == nil {
			return fmt.Errorf("safefileio: file has access ACL %s", attribute)
		}
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) {
			continue
		}
		return fmt.Errorf("inspect access ACL %s: %w", attribute, err)
	}
	return nil
}
