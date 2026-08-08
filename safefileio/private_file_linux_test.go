package safefileio

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
)

func TestLinuxFilesystemRequiresServerACL(t *testing.T) {
	cifs := uint32(unix.CIFS_SUPER_MAGIC)
	smb2 := uint32(unix.SMB2_SUPER_MAGIC)
	for name, filesystemType := range map[string]int64{
		"CIFS":               int64(cifs),
		"CIFS sign-extended": int64(int32(cifs)),
		"SMB":                unix.SMB_SUPER_MAGIC,
		"SMB2":               int64(smb2),
		"SMB2 sign-extended": int64(int32(smb2)),
	} {
		t.Run(name, func(t *testing.T) {
			assert.True(t, linuxFilesystemRequiresServerACL(filesystemType))
		})
	}
	assert.False(t, linuxFilesystemRequiresServerACL(unix.EXT4_SUPER_MAGIC))
}
