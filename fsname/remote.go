package fsname

// Remote reports whether path lives on a network file system (NFS, SMB/CIFS,
// AFS, Ceph, Coda, NCP, 9P, WebDAV, AFP and similar) or a user-space (FUSE)
// file system. On those, access checks, locking and durability are enforced
// by a server or a user-space process rather than the local kernel.
//
// Remote inspects the file system that contains path, following symbolic
// links: a link on a local disk that points into a network mount is remote.
// It reports the file system's location, not whether path is portable. A
// missing path returns the error. On platforms where kit cannot tell, Remote
// returns an error wrapping errors.ErrUnsupported instead of guessing.
//
// On Windows, Remote reports a volume whose drive type is DRIVE_REMOTE
// (network shares, mapped drives, and mount folders onto them). User-space
// file systems such as WinFsp are not reliably detectable and report false.
func Remote(path string) (bool, error) {
	return remote(path)
}
