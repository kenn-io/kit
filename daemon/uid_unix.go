//go:build !windows

package daemon

import (
	"os"
	"strconv"
)

func runtimeUID() string {
	return strconv.Itoa(os.Getuid())
}
