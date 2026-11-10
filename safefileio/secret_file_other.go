//go:build !unix && !windows

package safefileio

import (
	"context"
	"fmt"
	"os"
	"runtime"
)

// OpenSecretFile fails closed where secret access cannot be verified.
func OpenSecretFile(ctx context.Context, _ string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("safefileio: secret files are unsupported on %s", runtime.GOOS)
}
