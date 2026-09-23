//go:build !windows

package atomicfile

import "path/filepath"

// realDir returns dir with every symlink in it resolved, which is the
// directory a link inside dir resolves its relative destination against.
func realDir(dir string) (string, error) {
	return filepath.EvalSymlinks(dir)
}
