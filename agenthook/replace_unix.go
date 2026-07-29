//go:build !windows

package agenthook

import "os"

func replaceConfigFile(staging, target string) error {
	return os.Rename(staging, target)
}
