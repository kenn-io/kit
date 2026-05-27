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
	sidText := sid.String()
	sum := sha256.Sum256([]byte(sidText))
	return "sid-" + hex.EncodeToString(sum[:8])
}
