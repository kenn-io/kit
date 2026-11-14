//go:build !unix && !windows

package safefileio

import (
	"fmt"
	"os"
	"runtime"
)

// OpenSecretFile fails closed where secret access cannot be verified.
func OpenSecretFile(_ string) (*os.File, error) {
	return nil, fmt.Errorf("safefileio: secret files are unsupported on %s", runtime.GOOS)
}
