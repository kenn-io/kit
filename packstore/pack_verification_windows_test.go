//go:build windows

package packstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

const windowsFileDeleteChild = 0x40

func TestWindowsPackingReadableNonDeletableLooseCandidate(t *testing.T) {
	layout := layoutForStoreTest(t)
	content := []byte("readable packing candidate without deletion permission")
	hash := writeMaintenanceLoose(t, layout, content)
	loosePath := layout.LoosePath(hash)
	catalog := newMaintenanceCatalog()
	catalog.addLoose(hash, loosePath)
	restoreWindowsFileDACL := denyWindowsFileDeletion(t, loosePath)
	t.Cleanup(restoreWindowsFileDACL)
	readable, _, err := openLooseFile(loosePath)
	require.NoError(t, err, "fixture remains readable")
	require.NoError(t, readable.Close())
	deletePin, _, deleteErr := openLooseIdentityPin(loosePath)
	if deletePin != nil {
		require.NoError(t, deletePin.Close())
	}
	require.Error(t, deleteErr, "fixture must deny deletion-capable identity handles")
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1, stats.BlobsPacked)
	assert.Zero(t, stats.BlobsCorrupt)
	location, err := catalog.Resolve(context.Background(), hash)
	require.NoError(t, err)
	require.NotNil(t, location.Pack)
	assert.FileExists(t, loosePath)
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(t, content, got)
}

func TestWindowsRecoveryPacksReadableNonDeletableLooseAuthority(t *testing.T) {
	layout := layoutForStoreTest(t)
	content := []byte("readable loose authority without deletion permission")
	entry := buildStoreTestPack(t, layout, content)
	require.Equal(t, entry.Hash, writeMaintenanceLoose(t, layout, content))
	catalog := newMaintenanceCatalog()
	catalog.entries[entry.Hash] = entry
	catalog.members[entry.Hash] = Reference{Hash: entry.Hash, OriginalHashes: []string{entry.Hash.String()}}
	catalog.packs[entry.PackID] = PackRecord{
		PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now(),
	}
	packFile, err := os.OpenFile(layout.PackPath(entry.PackID), os.O_RDWR, 0)
	require.NoError(t, err)
	var damaged [1]byte
	_, err = packFile.ReadAt(damaged[:], entry.Offset)
	require.NoError(t, err)
	damaged[0] ^= 0xff
	_, err = packFile.WriteAt(damaged[:], entry.Offset)
	require.NoError(t, err)
	require.NoError(t, packFile.Close())
	loosePath := layout.LoosePath(entry.Hash)
	restoreWindowsFileDACL := denyWindowsFileDeletion(t, loosePath)
	t.Cleanup(restoreWindowsFileDACL)
	readable, _, err := openLooseFile(loosePath)
	require.NoError(t, err, "fixture remains readable")
	require.NoError(t, readable.Close())
	deletePin, _, deleteErr := openLooseIdentityPin(loosePath)
	if deletePin != nil {
		require.NoError(t, deletePin.Close())
	}
	require.Error(t, deleteErr, "fixture must deny deletion-capable identity handles")
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(context.Background(), PackOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1, stats.BlobsPacked)
	assert.Zero(t, stats.BlobsCorrupt)
	entries, _ := catalog.snapshot()
	require.Contains(t, entries, entry.Hash)
	assert.NotEqual(t, entry.PackID, entries[entry.Hash].PackID)
	assert.FileExists(t, loosePath)
	got, _ := readStoreTest(t, maintainer.store, entry.Hash)
	assert.Equal(t, content, got)
}

func denyWindowsFileDeletion(t *testing.T, path string) func() {
	t.Helper()
	file, err := openWindowsNoFollow(path, windows.READ_CONTROL|windows.WRITE_DAC)
	require.NoError(t, err)
	parent, err := openWindowsNoFollow(filepath.Dir(path), windows.READ_CONTROL|windows.WRITE_DAC)
	require.NoError(t, err)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)
	trustee := windows.TRUSTEE{
		TrusteeForm:  windows.TRUSTEE_IS_SID,
		TrusteeType:  windows.TRUSTEE_IS_USER,
		TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
	}
	restricted, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.DELETE,
			AccessMode:        windows.DENY_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee:           trustee,
		},
		{
			AccessPermissions: windows.GENERIC_READ,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee:           trustee,
		},
	}, nil)
	require.NoError(t, err)
	require.NoError(t, windows.SetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		restricted,
		nil,
	))
	restrictedParent, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windowsFileDeleteChild,
			AccessMode:        windows.DENY_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee:           trustee,
		},
		{
			AccessPermissions: windows.GENERIC_READ | windows.GENERIC_EXECUTE,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee:           trustee,
		},
	}, nil)
	require.NoError(t, err)
	require.NoError(t, windows.SetSecurityInfo(
		windows.Handle(parent.Fd()),
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		restrictedParent,
		nil,
	))
	return func() {
		fullControl, aclErr := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee:           trustee,
		}}, nil)
		require.NoError(t, aclErr)
		require.NoError(t, windows.SetSecurityInfo(
			windows.Handle(file.Fd()),
			windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
			nil,
			nil,
			fullControl,
			nil,
		))
		require.NoError(t, windows.SetSecurityInfo(
			windows.Handle(parent.Fd()),
			windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
			nil,
			nil,
			fullControl,
			nil,
		))
		require.NoError(t, file.Close())
		require.NoError(t, parent.Close())
	}
}
