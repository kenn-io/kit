//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package managedworktree

import (
	"fmt"
	"os"
	"syscall"
)

func repositoryFilesystemIdentity(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("filesystem metadata has unexpected type %T", info.Sys())
	}
	return fmt.Sprintf("unix:%v:%v", stat.Dev, stat.Ino), nil
}
