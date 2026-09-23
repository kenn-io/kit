//go:build windows

package fslink_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/kit/fslink"
)

func TestCreateJunctionResolvesRelativeTargetFromWorkingDir(t *testing.T) {
	assert := assert.New(t)
	dir := newTree(t)
	t.Chdir(dir)

	require.NoError(t, fslink.CreateJunction("sub", "link"))
	target, err := fslink.Readlink(filepath.Join(dir, "link"))
	require.NoError(t, err)
	assert.Equal(filepath.Join(dir, "sub"), target)
	assertFileContent(t, filepath.Join(dir, "link", "inner.txt"), "inner")
}

func TestCreateJunctionRejectsNonDirectoryTargets(t *testing.T) {
	for _, target := range []string{"file.txt", "missing"} {
		t.Run(target, func(t *testing.T) {
			dir := newTree(t)
			link := filepath.Join(dir, "link")
			err := fslink.CreateJunction(filepath.Join(dir, target), link)
			require.Error(t, err)
			_, statErr := os.Lstat(link)
			assert.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}
}

// os.Symlink would create a file symlink for a missing target, which Windows
// refuses to traverse once the directory exists.
func TestLinkDirCreatesDirectorySymlinkBeforeTargetExists(t *testing.T) {
	require := require.New(t)
	dir := newTree(t)
	symlinkOrSkip(t, "sub", filepath.Join(dir, "probe"))
	link := filepath.Join(dir, "link")

	kind, err := fslink.LinkDir("later", link)
	require.NoError(err)
	require.Equal(fslink.Symlink, kind)
	require.NoError(os.Mkdir(filepath.Join(dir, "later"), 0o755))
	require.NoError(os.WriteFile(filepath.Join(dir, "later", "x.txt"), []byte("later"), 0o600))

	assertFileContent(t, filepath.Join(link, "x.txt"), "later")
}

// A WOF-compressed file is a reparse point that is not a link. Its content is
// served by a filter driver that FILE_FLAG_OPEN_REPARSE_POINT bypasses, so
// opens must reach the file itself, not the stub.
func TestOpenNonLinkReparsePointReadsFileContent(t *testing.T) {
	content := strings.Repeat("compressible content ", 4096)
	newWOFFile := func(t *testing.T) string {
		t.Helper()
		require := require.New(t)
		path := filepath.Join(t.TempDir(), "compressed.txt")
		require.NoError(os.WriteFile(path, []byte(content), 0o600))
		out, err := exec.CommandContext(t.Context(), "compact.exe", "/c", "/exe:xpress4k", path).CombinedOutput()
		require.NoError(err, "compact: %s", out)
		path16, err := windows.UTF16PtrFromString(path)
		require.NoError(err)
		attrs, err := windows.GetFileAttributes(path16)
		require.NoError(err)
		if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
			t.Skipf("compact did not make a WOF reparse point: %s", out)
		}
		kind, err := fslink.Classify(path)
		require.NoError(err)
		require.Equal(fslink.NotLink, kind)
		return path
	}

	t.Run("ReadFile", func(t *testing.T) {
		data, err := fslink.ReadFile(newWOFFile(t))
		require.NoError(t, err)
		assert.Equal(t, content, string(data))
	})
	t.Run("OpenFile", func(t *testing.T) {
		file, err := fslink.OpenFile(newWOFFile(t), os.O_RDONLY, 0)
		require.NoError(t, err)
		defer func() { _ = file.Close() }()
		assert.Equal(t, content, readAll(t, file))
	})
	t.Run("OpenFile truncates the file", func(t *testing.T) {
		require := require.New(t)
		path := newWOFFile(t)
		file, err := fslink.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
		require.NoError(err)
		_, err = file.WriteString("new")
		require.NoError(err)
		require.NoError(file.Close())
		assertFileContent(t, path, "new")
	})
}
