//go:build windows

package packstore

import (
	"bytes"
	"context"
	"os"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestStoreOpenWindowsTemporaryRejectsWritersAndPreservesReplacement(t *testing.T) {
	content := bytes.Repeat([]byte("verified seekable Windows content "), 1024)
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		hash: {Member: true},
	}}, layout)

	reader, _, err := store.Open(context.Background(), hash)
	require.NoError(t, err)
	named, ok := reader.(interface{ Name() string })
	require.True(t, ok)
	temporaryPath := named.Name()
	t.Cleanup(func() { _ = os.Remove(temporaryPath) })

	name, err := windows.UTF16PtrFromString(temporaryPath)
	require.NoError(t, err)
	readerHandle, openErr := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if openErr == nil {
		require.NoError(t, windows.CloseHandle(readerHandle))
	}
	require.ErrorIs(t, openErr, windows.ERROR_SHARING_VIOLATION)

	writer, openErr := windows.CreateFile(
		name,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if openErr == nil {
		require.NoError(t, windows.CloseHandle(writer))
	}
	require.ErrorIs(t, openErr, windows.ERROR_SHARING_VIOLATION)
	handleSource, ok := reader.(interface{ Fd() uintptr })
	require.True(t, ok)
	assertWindowsSeekableTempDACL(t, windows.Handle(handleSource.Fd()))

	displaced := temporaryPath + ".displaced"
	require.NoError(t, os.Rename(temporaryPath, displaced))
	t.Cleanup(func() { _ = os.Remove(displaced) })
	replacement := []byte("unrelated replacement remains")
	require.NoError(t, os.WriteFile(temporaryPath, replacement, 0o600))

	require.NoError(t, reader.Close())

	assert.NoFileExists(t, displaced, "closing the reader deletes only its renamed temporary file")
	assert.Equal(t, replacement, mustReadFile(t, temporaryPath))
}

func assertWindowsSeekableTempDACL(t *testing.T, handle windows.Handle) {
	t.Helper()
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	require.NoError(t, err)
	control, _, err := descriptor.Control()
	require.NoError(t, err)
	assert.NotZero(t, control&windows.SE_DACL_PROTECTED)
	dacl, _, err := descriptor.DACL()
	require.NoError(t, err)
	require.NotNil(t, dacl)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	require.NoError(t, err)
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	require.NoError(t, err)
	allowed := []*windows.SID{user.User.Sid, system, admins}
	require.Positive(t, dacl.AceCount)
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		require.NoError(t, windows.GetAce(dacl, uint32(index), &ace))
		require.Equal(t, uint8(windows.ACCESS_ALLOWED_ACE_TYPE), ace.Header.AceType)
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		assert.Condition(t, func() bool {
			for _, trusted := range allowed {
				if sid.Equals(trusted) {
					return true
				}
			}
			return false
		}, "temporary DACL grants access only to trusted principals")
	}
}
