package pathresolve_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pathresolve"
)

func TestEvalSymlinksMatchesFilepath(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b")
	require.NoError(t, os.MkdirAll(nested, 0o755))

	paths := []string{dir, filepath.Join(dir, "a"), nested, filepath.Join(dir, "a", ".")}
	for _, path := range paths {
		want, err := filepath.EvalSymlinks(path)
		require.NoError(t, err)

		got, err := pathresolve.EvalSymlinks(path)
		require.NoError(t, err)
		assert.Equal(t, want, got, "resolving %s", path)
	}
}

func TestEvalSymlinksMissingPathReportsFilepathError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")

	_, wantErr := filepath.EvalSymlinks(missing)
	require.Error(t, wantErr)

	got, err := pathresolve.EvalSymlinks(missing)
	require.Error(t, err)
	assert.Empty(t, got)
	assert.Equal(t, wantErr, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestEvalSymlinksNonDirectoryElementFails(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	got, err := pathresolve.EvalSymlinks(filepath.Join(file, "child"))
	require.Error(t, err)
	assert.Empty(t, got)
}
