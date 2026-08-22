//go:build windows

package packstore

import (
	"bytes"
	"context"
	"os"
	"testing"
	"unsafe"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestStoreOpenWindowsTemporaryRejectsWritersAndPreservesReplacement(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	content := bytes.Repeat([]byte("verified seekable Windows content "), 1024)
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		hash: {Member: true},
	}}, layout)

	reader, _, err := store.Open(context.Background(), hash)
	require.NoError(err)
	named, ok := reader.(interface{ Name() string })
	require.True(ok)
	temporaryPath := named.Name()
	t.Cleanup(func() { _ = os.Remove(temporaryPath) })

	name, err := windows.UTF16PtrFromString(temporaryPath)
	require.NoError(err)
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
		require.NoError(windows.CloseHandle(readerHandle))
	}
	require.ErrorIs(openErr, windows.ERROR_SHARING_VIOLATION)

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
		require.NoError(windows.CloseHandle(writer))
	}
	require.ErrorIs(openErr, windows.ERROR_SHARING_VIOLATION)
	handleSource, ok := reader.(interface{ Fd() uintptr })
	require.True(ok)
	assertWindowsSeekableTempDACL(t, windows.Handle(handleSource.Fd()))

	displaced := temporaryPath + ".displaced"
	require.NoError(os.Rename(temporaryPath, displaced))
	t.Cleanup(func() { _ = os.Remove(displaced) })
	replacement := []byte("unrelated replacement remains")
	require.NoError(os.WriteFile(temporaryPath, replacement, 0o600))

	require.NoError(reader.Close())

	assert.NoFileExists(displaced, "closing the reader deletes only its renamed temporary file")
	assert.Equal(replacement, mustReadFile(t, temporaryPath))
}

func assertWindowsSeekableTempDACL(t *testing.T, handle windows.Handle) {
	t.Helper()
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	Require.NoError(t, err)
	control, _, err := descriptor.Control()
	Require.NoError(t, err)
	Assert.NotZero(t, control&windows.SE_DACL_PROTECTED)
	dacl, _, err := descriptor.DACL()
	Require.NoError(t, err)
	Require.NotNil(t, dacl)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	Require.NoError(t, err)
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	Require.NoError(t, err)
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	Require.NoError(t, err)
	allowed := []*windows.SID{user.User.Sid, system, admins}
	Require.Positive(t, dacl.AceCount)
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		Require.NoError(t, windows.GetAce(dacl, uint32(index), &ace))
		Require.Equal(t, uint8(windows.ACCESS_ALLOWED_ACE_TYPE), ace.Header.AceType)
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		Assert.Condition(t, func() bool {
			for _, trusted := range allowed {
				if sid.Equals(trusted) {
					return true
				}
			}
			return false
		}, "temporary DACL grants access only to trusted principals")
	}
}
