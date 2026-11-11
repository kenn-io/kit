package safefileio_test

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
	"golang.org/x/sys/unix"
)

func secretAccessACL() []byte {
	// POSIX access ACL v2: the named user's entry is masked to no access.
	data := make([]byte, 4+5*8)
	binary.LittleEndian.PutUint32(data, 2)
	for i, tag := range []uint16{1, 2, 4, 16, 32} {
		entry := data[4+i*8:]
		binary.LittleEndian.PutUint16(entry, tag)
		binary.LittleEndian.PutUint32(entry[4:], ^uint32(0))
		if tag == 1 {
			binary.LittleEndian.PutUint16(entry[2:], 7)
		}
		if tag == 2 {
			binary.LittleEndian.PutUint32(entry[4:], uint32(os.Geteuid()+1))
		}
	}
	return data
}

func TestSecretLinuxAccessACLBoundary(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(path, []byte("secret"), 0o600))
	acl := secretAccessACL()
	// Files need rw, not directory traversal, in their owner entry.
	binary.LittleEndian.PutUint16(acl[6:], 6)
	err := unix.Setxattr(path, "system.posix_acl_access", acl, 0)
	if errors.Is(err, unix.ENOTSUP) {
		t.Skip("fixture filesystem does not support POSIX ACLs")
	}
	require.NoError(t, err)
	file, err := safefileio.OpenSecretFile(path)
	require.Error(t, err)
	assert.Nil(t, file)
	require.NoError(t, unix.Removexattr(path, "system.posix_acl_access"))
	// Harmless parent ACLs retain the existing Linux mask-based policy.
	require.NoError(t, unix.Setxattr(dir, "system.posix_acl_access", secretAccessACL(), 0))
	file, err = safefileio.OpenSecretFile(path)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}
