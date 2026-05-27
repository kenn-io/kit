//go:build windows

package daemon

func runtimeUID() string {
	return "user"
}
