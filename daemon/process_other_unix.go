//go:build !linux && !windows

package daemon

import "errors"

func listedProcessCreateTime(_ int) (int64, error) {
	return 0, errors.ErrUnsupported
}

func processHasExited(_ int) bool {
	return false
}
