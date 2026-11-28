//go:build windows

package safefileio

import (
	"errors"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

type tokenOwner struct {
	Owner *windows.SID
}

func currentWindowsUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	if user.User.Sid == nil {
		return nil, errors.New("current token user is missing")
	}
	sid, err := user.User.Sid.Copy()
	runtime.KeepAlive(user)
	return sid, err
}

func currentWindowsOwnerSID() (*windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	n := uint32(64)
	for {
		buf := make([]byte, n)
		err := windows.GetTokenInformation(token, windows.TokenOwner, &buf[0], uint32(len(buf)), &n)
		if err == nil {
			owner := (*tokenOwner)(unsafe.Pointer(&buf[0]))
			if owner.Owner == nil {
				return nil, errors.New("current token owner is missing")
			}
			sid, err := owner.Owner.Copy()
			runtime.KeepAlive(buf)
			return sid, err
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return nil, err
		}
		if n <= uint32(len(buf)) {
			return nil, err
		}
	}
}

func windowsOwnerMatches(owner *windows.SID, allowed ...*windows.SID) bool {
	if owner == nil {
		return false
	}
	for _, sid := range allowed {
		if sid != nil && owner.Equals(sid) {
			return true
		}
	}
	return false
}
