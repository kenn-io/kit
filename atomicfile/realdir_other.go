//go:build !windows

package atomicfile

import (
	"path/filepath"
	"strings"
)

// resolveLinkDest returns the path named by a link in linkDir whose
// destination is dest, resolved the way the kernel resolves it: a relative
// dest starts at linkDir's real directory, and every ".." applies after the
// link before it is resolved. filepath.Join would apply ".." lexically first,
// so `hop/../target` with hop a directory symlink would name the wrong file.
// Instead the destination is kept unclean, split before its last element,
// and its parent part resolved with filepath.EvalSymlinks, which resolves
// each link before applying the ".." after it.
func resolveLinkDest(linkDir, dest string) (string, error) {
	full := dest
	if !filepath.IsAbs(dest) {
		parent, err := filepath.EvalSymlinks(linkDir)
		if err != nil {
			return "", err
		}
		full = parent + "/" + dest
	}
	i := strings.LastIndexByte(full, '/')
	parent, base := full[:i+1], full[i+1:]
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, base), nil
}
