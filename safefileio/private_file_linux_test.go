package safefileio

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
)

func TestLinuxFilesystemHasExternalAccessPolicy(t *testing.T) {
	for name, magic := range map[string]uint32{
		"Andrew FS":         uint32(unix.AAFS_MAGIC),
		"AFS fs":            uint32(unix.AFS_FS_MAGIC),
		"AFS":               uint32(unix.AFS_SUPER_MAGIC),
		"Ceph":              uint32(unix.CEPH_SUPER_MAGIC),
		"CIFS":              uint32(unix.CIFS_SUPER_MAGIC),
		"Coda":              uint32(unix.CODA_SUPER_MAGIC),
		"FUSE":              uint32(unix.FUSE_SUPER_MAGIC),
		"NCP":               uint32(unix.NCP_SUPER_MAGIC),
		"NFS":               uint32(unix.NFS_SUPER_MAGIC),
		"SMB":               uint32(unix.SMB_SUPER_MAGIC),
		"SMB2":              uint32(unix.SMB2_SUPER_MAGIC),
		"Plan 9 filesystem": uint32(unix.V9FS_MAGIC),
	} {
		t.Run(name, func(t *testing.T) {
			assert.True(t, linuxFilesystemHasExternalAccessPolicy(int64(magic)))
			assert.True(t, linuxFilesystemHasExternalAccessPolicy(int64(int32(magic))))
		})
	}
	assert.False(t, linuxFilesystemHasExternalAccessPolicy(unix.EXT4_SUPER_MAGIC))
}
