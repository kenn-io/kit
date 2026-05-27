//go:build windows

package daemon

import (
	"crypto/sha256"
	"encoding/hex"
)

func runtimeUID() string {
	sid, err := currentWindowsUserSID()
	if err != nil {
		return "unknown"
	}
	sum := sha256.Sum256([]byte(sid.String()))
	return "sid-" + hex.EncodeToString(sum[:8])
}
