//go:build windows

package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestEnsurePrivateRuntimeDirCreatesOwnedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime")

	require.NoError(t, ensurePrivateRuntimeDir(dir))

	userSID, err := currentWindowsUserSID()
	require.NoError(t, err)
	descriptor, err := windows.GetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	require.NoError(t, err)
	owner, _, err := descriptor.Owner()
	require.NoError(t, err)
	require.NotNil(t, owner)
	require.True(t, owner.Equals(userSID))
}

func TestEnsurePrivateRuntimeDirRejectsSymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	link := filepath.Join(base, "link")
	require.NoError(t, os.Mkdir(target, 0o700))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	require.Error(t, ensurePrivateRuntimeDir(link))
}

func TestRuntimeUIDIsPerUser(t *testing.T) {
	require.NotEmpty(t, runtimeUID())
	require.NotEqual(t, "user", runtimeUID())
	require.Contains(t, runtimeUID(), "sid-")
}
