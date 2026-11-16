//go:build unix

package safefileio

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscardCreatedFileRemovesCreatedFile(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	require.NoError(os.Chmod(dir, 0o700))
	path := filepath.Join(dir, "record.json")
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
		name       string
		mode       fs.FileMode
		createdUID int
		wantErr    string
		parent     *ownedInfo
	}{
		{name: "private", mode: 0o700, createdUID: os.Getuid()},
		{name: "group readable", mode: 0o750, createdUID: os.Getuid()},
		{name: "sticky shared", mode: 0o777 | fs.ModeSticky, createdUID: os.Getuid()},
		{name: "sticky shared with root-owned file", mode: 0o777 | fs.ModeSticky, createdUID: 0},
		{name: "sticky shared with foreign file", mode: 0o777 | fs.ModeSticky, createdUID: os.Getuid() + 1, wantErr: "created file has an untrusted owner"},
		{name: "group writable", mode: 0o770, createdUID: os.Getuid(), wantErr: "other users can rename entries"},
		{name: "world writable", mode: 0o777, createdUID: os.Getuid(), wantErr: "other users can rename entries"},
		{name: "foreign parent", parent: &ownedInfo{uid: 2, mode: os.ModeDir | 0o700}, wantErr: "owned by another user"},
		{name: "private root", parent: &ownedInfo{uid: 0, mode: os.ModeDir | 0o755}},
		{name: "not a directory", parent: &ownedInfo{uid: 1, mode: 0o700}, wantErr: "is not a directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			var err error
			if tt.parent != nil {
				err = requireUnsharedParentForUID(*tt.parent, ownedInfo{uid: 1}, 1)
			} else {
				dir := t.TempDir()
				require.NoError(os.Chmod(dir, tt.mode))
				dirInfo, statErr := os.Stat(dir)
				require.NoError(statErr)
				err = requireUnsharedParent(dirInfo, ownedInfo{uid: uint32(tt.createdUID)})
			}

			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// ownedInfo supplies file metadata for owners a test process cannot create files as.
type ownedInfo struct {
	uid  uint32
	mode fs.FileMode
}

func (i ownedInfo) Name() string       { return "created" }
func (i ownedInfo) Size() int64        { return 0 }
func (i ownedInfo) Mode() fs.FileMode  { return i.mode }
func (i ownedInfo) ModTime() time.Time { return time.Time{} }
func (i ownedInfo) IsDir() bool        { return i.mode.IsDir() }
func (i ownedInfo) Sys() any           { return &syscall.Stat_t{Uid: i.uid} }
