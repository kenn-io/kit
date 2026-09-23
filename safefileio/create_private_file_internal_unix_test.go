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

func TestDiscardCreatedFileLeavesFileInSharedParent(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	require.NoError(os.Chmod(dir, 0o777))
	path := filepath.Join(dir, "record.json")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	require.NoError(err)

	require.ErrorContains(discardCreatedFile(path, file), "other users can rename entries")

	_, err = os.Lstat(path)
	require.NoError(err)
}

func TestRequireUnsharedParent(t *testing.T) {
	tests := []struct {
		name    string
		mode    fs.FileMode
		wantErr string
	}{
		{name: "private", mode: 0o700},
		{name: "group readable", mode: 0o750},
		{name: "sticky shared", mode: 0o777 | fs.ModeSticky},
		{name: "group writable", mode: 0o770, wantErr: "other users can rename entries"},
		{name: "world writable", mode: 0o777, wantErr: "other users can rename entries"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Chmod(dir, tt.mode))

			err := requireUnsharedParent(dir)

			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}
