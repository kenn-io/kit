//go:build windows

package packstore

import (
	"bytes"
	"context"
	"os"
	"testing"

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

	displaced := temporaryPath + ".displaced"
	require.NoError(t, os.Rename(temporaryPath, displaced))
	t.Cleanup(func() { _ = os.Remove(displaced) })
	replacement := []byte("unrelated replacement remains")
	require.NoError(t, os.WriteFile(temporaryPath, replacement, 0o600))

	require.NoError(t, reader.Close())

	assert.NoFileExists(t, displaced, "closing the reader deletes only its renamed temporary file")
	assert.Equal(t, replacement, mustReadFile(t, temporaryPath))
}
