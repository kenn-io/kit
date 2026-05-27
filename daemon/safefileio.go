package daemon

import (
	"os"

	"go.kenn.io/kit/safefileio"
)

func ensurePrivateRuntimeDir(path string) error {
	return safefileio.EnsurePrivateDir(path)
}

func openRuntimeFile(path string) (*os.File, error) {
	return safefileio.OpenCurrentUserFile(path)
}
