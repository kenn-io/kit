//go:build !windows

package daemon

import (
	"fmt"
	"os"
	"syscall"
)

func ensurePrivateRuntimeDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %s: missing owner information", path)
	}
	if stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%s is not owned by current user", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("chmod private runtime dir: %w", err)
		}
	}
	return nil
}
