//go:build windows

package packstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestRetirePackWindowsSharingViolationIsRetryable(t *testing.T) {
	layout := layoutForStoreTest(t)
	entry := buildStoreTestPack(t, layout, []byte("windows sharing retirement"))
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		entry.Hash: {Member: true, Pack: &entry},
	}}, layout)
	stream, _, err := store.OpenStream(context.Background(), entry.Hash)
	require.NoError(t, err)
	require.NoError(t, stream.Verify())
	require.NoError(t, stream.Close())

	name, err := windows.UTF16PtrFromString(layout.PackPath(entry.PackID))
	require.NoError(t, err)
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	err = store.RetirePack(entry.PackID)
	require.ErrorIs(t, err, ErrPackRetirementDeferred)
	require.NoError(t, windows.CloseHandle(handle))
	require.NoError(t, store.RetirePack(entry.PackID))
}
