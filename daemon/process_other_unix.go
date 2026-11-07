//go:build !linux && !windows

package daemon

func processHasExited(_ int) bool {
	return false
}
