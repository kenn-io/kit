package safefileio

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	darwinACLExtended   = 0x00000100
	darwinACLFirstEntry = 0
)

type darwinACLAPI struct {
	getFD    uintptr
	getEntry uintptr
	getTag   uintptr
	free     func(uintptr) int32
}

var (
	darwinACLOnce sync.Once
	darwinACL     darwinACLAPI
	errDarwinACL  error
)

// ValidatePrivateCurrentUserFile verifies that an open current-user-owned file
// has private mode bits and no macOS extended ACL.
func ValidatePrivateCurrentUserFile(file *os.File) error {
	return validatePrivateCurrentUserFile(file, validateDarwinExtendedACL)
}

func validateDarwinExtendedACL(file *os.File) error { return validateDarwinACL(file, false) }

func validateDarwinACL(file *os.File, allowDeny bool) error {
	darwinACLOnce.Do(loadDarwinACL)
	if errDarwinACL != nil {
		return errDarwinACL
	}
	// Capture errno before resuming Go: even a locked thread's errno can be
	// changed by the runtime between the native call and a later read.
	acl, _, errno := purego.SyscallN(darwinACL.getFD, file.Fd(), darwinACLExtended)
	runtime.KeepAlive(file)
	if acl == 0 {
		// Darwin errno is a C int; the native trampoline returns a word.
		callErr := syscall.Errno(uint32(errno))
		if callErr == syscall.ENOENT {
			return nil
		}
		return fmt.Errorf("read extended ACL: %w", callErr)
	}
	defer func() { _ = darwinACL.free(acl) }()
	entryID := uintptr(darwinACLFirstEntry)
	for {
		var entry uintptr
		result, _, errno := purego.SyscallN(darwinACL.getEntry, acl, entryID, uintptr(unsafe.Pointer(&entry)))
		if int32(result) != 0 {
			if allowDeny && syscall.Errno(uint32(errno)) == syscall.EINVAL {
				return nil
			}
			return errors.New("read extended ACL entry failed")
		}
		if !allowDeny {
			return errors.New("safefileio: file has a macOS extended ACL")
		}
		var tag int32
		result, _, _ = purego.SyscallN(darwinACL.getTag, entry, uintptr(unsafe.Pointer(&tag)))
		if int32(result) != 0 {
			return errors.New("read extended ACL tag failed")
		}
		// Darwin ACL_EXTENDED_DENY is 2; only restrictive entries can pass.
		if tag != 2 {
			return errors.New("safefileio: secret parent has an allowing ACL")
		}
		entryID = ^uintptr(0) // Darwin ACL_NEXT_ENTRY is -1.
	}
}

func loadDarwinACL() {
	handle, err := purego.Dlopen(
		"/usr/lib/libSystem.B.dylib",
		purego.RTLD_NOW|purego.RTLD_LOCAL,
	)
	if err != nil {
		errDarwinACL = fmt.Errorf("load macOS ACL API: %w", err)
		return
	}
	darwinACL.getFD, err = purego.Dlsym(handle, "acl_get_fd_np")
	if err != nil {
		errDarwinACL = fmt.Errorf("load macOS ACL function acl_get_fd_np: %w", err)
		return
	}
	for name, target := range map[string]*uintptr{"acl_get_entry": &darwinACL.getEntry, "acl_get_tag_type": &darwinACL.getTag} {
		*target, err = purego.Dlsym(handle, name)
		if err != nil {
			errDarwinACL = fmt.Errorf("load macOS ACL function %s: %w", name, err)
			return
		}
	}
	for name, target := range map[string]any{
		"acl_free": &darwinACL.free,
	} {
		symbol, symbolErr := purego.Dlsym(handle, name)
		err = symbolErr
		if err != nil {
			errDarwinACL = fmt.Errorf("load macOS ACL function %s: %w", name, err)
			return
		}
		purego.RegisterFunc(target, symbol)
	}
}
