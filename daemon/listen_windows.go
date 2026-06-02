//go:build windows

package daemon

func isStaleUnixSocketDialError(error) bool {
	return false
}
