//go:build windows

package daemon

import (
	"crypto/sha256"
	"encoding/hex"

	"golang.org/x/sys/windows"
)

func runtimeUID() string {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "unknown"
	}
	sum := sha256.Sum256([]byte(user.User.Sid.String()))
	return "sid-" + hex.EncodeToString(sum[:8])
}
