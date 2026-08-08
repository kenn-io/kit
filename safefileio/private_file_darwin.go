package safefileio

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"

	"github.com/ebitengine/purego"
)

const (
	darwinACLExtended   = 0x00000100
	darwinACLFirstEntry = 0
)

type darwinACLAPI struct {
	getFD    func(int32, int32) uintptr
	getEntry func(uintptr, int32, *uintptr) int32
	free     func(uintptr) int32
	errno    func() *int32
}

var (
	darwinACLOnce sync.Once
	darwinACL     darwinACLAPI
	darwinACLErr  error
)

// ValidatePrivateCurrentUserFile verifies that an open current-user-owned file
// has private mode bits and no macOS extended ACL.
func ValidatePrivateCurrentUserFile(file *os.File) error {
	return validatePrivateCurrentUserFile(file, validateDarwinExtendedACL)
}

func validateDarwinExtendedACL(file *os.File) error {
	darwinACLOnce.Do(loadDarwinACL)
	if darwinACLErr != nil {
		return darwinACLErr
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	errno := darwinACL.errno()
	*errno = 0
	acl := darwinACL.getFD(int32(file.Fd()), darwinACLExtended)
	if acl == 0 {
		callErr := syscall.Errno(*errno)
		if callErr == syscall.ENOENT {
			return nil
		}
		return fmt.Errorf("read extended ACL: %w", callErr)
	}
	defer func() { _ = darwinACL.free(acl) }()
	var entry uintptr
	result := darwinACL.getEntry(acl, darwinACLFirstEntry, &entry)
	switch result {
	case 0:
		return errors.New("safefileio: file has a macOS extended ACL")
	default:
		return errors.New("read extended ACL entry failed")
	}
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
	for name, target := range map[string]any{
		"acl_get_fd_np": &darwinACL.getFD,
		"acl_get_entry": &darwinACL.getEntry,
		"acl_free":      &darwinACL.free,
		"__error":       &darwinACL.errno,
	} {
		symbol, symbolErr := purego.Dlsym(handle, name)
		err = symbolErr
		if err != nil {
			darwinACLErr = fmt.Errorf("load macOS ACL function %s: %w", name, err)
			return
		}
		purego.RegisterFunc(target, symbol)
	}
}
