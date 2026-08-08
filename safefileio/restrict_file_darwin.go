package safefileio

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"

	"github.com/ebitengine/purego"
)

const darwinACLExtended = 0x00000100

type darwinACLAPI struct {
	init uintptr
	set  uintptr
	free uintptr
}

var (
	darwinACLOnce sync.Once
	darwinACL     darwinACLAPI
	darwinACLErr  error
)

// RestrictCurrentUserFile validates an open handle, removes its macOS extended
// ACL, and makes it readable and writable only by its current-user owner.
func RestrictCurrentUserFile(file *os.File) error {
	if err := ValidateCurrentUserFile(file); err != nil {
		return err
	}
	if err := removeDarwinExtendedACL(file); err != nil {
		return err
	}
	return file.Chmod(0o600)
}

func removeDarwinExtendedACL(file *os.File) error {
	darwinACLOnce.Do(loadDarwinACL)
	if darwinACLErr != nil {
		return darwinACLErr
	}
	acl, _, callErr := purego.SyscallN(darwinACL.init, 0)
	if acl == 0 {
		return darwinACLCallError("initialize empty ACL", callErr)
	}
	defer func() { _, _, _ = purego.SyscallN(darwinACL.free, acl) }()
	result, _, callErr := purego.SyscallN(
		darwinACL.set,
		file.Fd(),
		acl,
		darwinACLExtended,
	)
	if result != 0 {
		return darwinACLCallError("remove extended ACL", callErr)
	}
	return nil
}

func loadDarwinACL() {
	handle, err := purego.Dlopen(
		"/usr/lib/libSystem.B.dylib",
		purego.RTLD_NOW|purego.RTLD_LOCAL,
	)
	if err != nil {
		darwinACLErr = fmt.Errorf("load macOS ACL API: %w", err)
		return
	}
	for name, target := range map[string]*uintptr{
		"acl_init":      &darwinACL.init,
		"acl_set_fd_np": &darwinACL.set,
		"acl_free":      &darwinACL.free,
	} {
		*target, err = purego.Dlsym(handle, name)
		if err != nil {
			darwinACLErr = fmt.Errorf("load macOS ACL function %s: %w", name, err)
			return
		}
	}
}

func darwinACLCallError(operation string, value uintptr) error {
	if value != 0 {
		return fmt.Errorf("%s: %w", operation, syscall.Errno(value))
	}
	return errors.New(operation + " failed")
}
