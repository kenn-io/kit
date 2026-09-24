//go:build windows

package atomicfile_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/kit/atomicfile"
)

// openShareAll opens path for reading with FILE_SHARE_READ, FILE_SHARE_WRITE,
// and FILE_SHARE_DELETE, the way a reader that tolerates renames would.
func openShareAll(t *testing.T, path string) *os.File {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	f := os.NewFile(uintptr(h), path)
	t.Cleanup(func() { f.Close() })
	return f
}

func readAll(t *testing.T, f *os.File) string {
	t.Helper()
	_, err := f.Seek(0, io.SeekStart)
	require.NoError(t, err)
	data, err := io.ReadAll(f)
	require.NoError(t, err)
	return string(data)
}

// A reader holding the target open with FILE_SHARE_DELETE does not block the
// POSIX-semantics rename, and keeps reading the content it opened.
func TestReplaceTargetOpenWithShareDelete(t *testing.T) {
	openers := map[string]func(t *testing.T, dir string) *os.File{
		"CreateFile": func(t *testing.T, dir string) *os.File {
			t.Helper()
			return openShareAll(t, filepath.Join(dir, "dst"))
		},
		"os.Root": func(t *testing.T, dir string) *os.File {
			t.Helper()
			root, err := os.OpenRoot(dir)
			require.NoError(t, err)
			t.Cleanup(func() { root.Close() })
			f, err := root.Open("dst")
			require.NoError(t, err)
			t.Cleanup(func() { f.Close() })
			return f
		},
	}
	for name, open := range openers {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			src := writeString(t, dir, "src", "new")
			dst := writeString(t, dir, "dst", "old")
			held := open(t, dir)

			require.NoError(t, atomicfile.Replace(src, dst))

			assert.Equal(t, "new", readString(t, dst))
			assert.Equal(t, "old", readAll(t, held))
			assert.Equal(t, []string{"dst"}, entryNames(t, dir))
		})
	}
}

// os.Open shares read and write but not delete, so a reader holding the
// target that way still blocks Replace.
func TestReplaceTargetOpenWithoutShareDelete(t *testing.T) {
	dir := t.TempDir()
	src := writeString(t, dir, "src", "new")
	dst := writeString(t, dir, "dst", "old")
	held, err := os.Open(dst)
	require.NoError(t, err)
	defer held.Close()

	err = atomicfile.Replace(src, dst)

	require.ErrorIs(t, err, windows.ERROR_ACCESS_DENIED)
	assert.Equal(t, "new", readString(t, src))
	assert.Equal(t, "old", readString(t, dst))
}

func TestReplaceSourceOpen(t *testing.T) {
	t.Run("with FILE_SHARE_DELETE", func(t *testing.T) {
		dir := t.TempDir()
		src := writeString(t, dir, "src", "new")
		dst := writeString(t, dir, "dst", "old")
		held := openShareAll(t, src)

		require.NoError(t, atomicfile.Replace(src, dst))

		assert.Equal(t, "new", readString(t, dst))
		assert.Equal(t, "new", readAll(t, held))
		assert.Equal(t, []string{"dst"}, entryNames(t, dir))
	})
	t.Run("without FILE_SHARE_DELETE", func(t *testing.T) {
		dir := t.TempDir()
		src := writeString(t, dir, "src", "new")
		dst := writeString(t, dir, "dst", "old")
		held, err := os.Open(src)
		require.NoError(t, err)
		defer held.Close()

		err = atomicfile.Replace(src, dst)

		require.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)
		assert.Equal(t, "new", readString(t, src))
		assert.Equal(t, "old", readString(t, dst))
	})
}

func TestReplaceDirectoryOverDirectory(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	require.NoError(t, os.Mkdir(src, 0o700))
	writeString(t, src, "inner", "new")
	dst := filepath.Join(dir, "dst")
	require.NoError(t, os.Mkdir(dst, 0o700))

	err := atomicfile.Replace(src, dst)

	require.ErrorIs(t, err, windows.ERROR_ACCESS_DENIED)
	assert.Equal(t, "new", readString(t, filepath.Join(src, "inner")))
	assert.Empty(t, entryNames(t, dst))
}

// A junction is a directory, so Replace refuses it like any directory, and
// never writes through it.
func TestReplaceJunctionTarget(t *testing.T) {
	dir, link, target := newJunction(t)
	src := writeString(t, t.TempDir(), "src", "new")

	err := atomicfile.Replace(src, link)

	require.ErrorIs(t, err, windows.ERROR_ACCESS_DENIED)
	assert.Equal(t, "new", readString(t, src))
	assertJunctionUntouched(t, dir, link, target)
}

func TestReplaceRelativePaths(t *testing.T) {
	dir := t.TempDir()
	writeString(t, dir, "src", "new")
	writeString(t, dir, "dst", "old")
	t.Chdir(dir)

	require.NoError(t, atomicfile.Replace("src", "dst"))

	assert.Equal(t, "new", readString(t, filepath.Join(dir, "dst")))
	assert.Equal(t, []string{"dst"}, entryNames(t, dir))
}
