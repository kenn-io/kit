package safefileio

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
)

func TestLinuxFilesystemRequiresServerACL(t *testing.T) {
	for name, filesystemType := range map[string]int64{
		"CIFS": unix.CIFS_SUPER_MAGIC,
		"SMB":  unix.SMB_SUPER_MAGIC,
		"SMB2": unix.SMB2_SUPER_MAGIC,
	} {
		t.Run(name, func(t *testing.T) {
			assert.True(t, linuxFilesystemRequiresServerACL(filesystemType))
		})
	}
	assert.False(t, linuxFilesystemRequiresServerACL(unix.EXT4_SUPER_MAGIC))
}
