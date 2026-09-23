//go:build unix

package safefileio

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscardCreatedFileRemovesCreatedFile(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "record.json")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	require.NoError(err)

	require.NoError(discardCreatedFile(path, file))

	_, err = os.Lstat(path)
	require.ErrorIs(err, fs.ErrNotExist)
}

func TestDiscardCreatedFileKeepsReplacedEntry(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	require.NoError(err)
	require.NoError(os.Rename(path, filepath.Join(dir, "moved.json")))
	require.NoError(os.WriteFile(path, []byte("replacement"), 0o600))

	require.Error(discardCreatedFile(path, file))

	data, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(t, "replacement", string(data))
}
