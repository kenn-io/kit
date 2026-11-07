//go:build unix

package agenthook

import (
	"os"
	"path/filepath"
	"syscall"
)

func claudeRootSettingsOwned(root string) bool {
	for _, entry := range []string{"", ".git", ".claude"} {
		stat := os.Lstat
		if entry == "" {
			stat = os.Stat
		}
		info, err := stat(filepath.Join(root, entry))
		if err != nil {
			return entry == ".claude" && os.IsNotExist(err)
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uint32(os.Geteuid()) {
			return false
		}
	}
	return true
}
