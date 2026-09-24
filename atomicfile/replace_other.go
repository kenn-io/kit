//go:build !windows

package atomicfile

import "os"

// Replace renames oldpath to newpath, atomically replacing a file already at
// newpath. It never copies, so a rename across volumes fails. A link at
// either path is renamed or replaced itself, never followed.
//
// On Unix it is os.Rename; rename(2) fails with EXDEV across devices. On
// Windows it first renames with POSIX semantics, which succeeds while another
// handle holds newpath open with FILE_SHARE_DELETE (the holder keeps reading
// the old content), and falls back to MoveFileEx where that is unsupported. A
// handle opened there without FILE_SHARE_DELETE, as os.Open opens files,
// still makes Replace fail.
func Replace(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

// replaceFile renames staging over target for WriteFile and Commit.
func replaceFile(staging, target string) error {
	return Replace(staging, target)
}
