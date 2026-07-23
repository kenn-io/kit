//go:build js || plan9 || wasip1

package managedworktree

import (
	"path/filepath"
	"strings"
)

func repositoryFilesystemIdentity(path string) (string, error) {
	return "path:" + strings.ToLower(filepath.Clean(path)), nil
}
