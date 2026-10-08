package pathresolve_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/fslink"
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

func TestEvalSymlinksAllowMissingFollowsLinkChain(t *testing.T) {
	dir := t.TempDir()
	canonical, err := pathresolve.EvalSymlinks(dir)
	require.NoError(t, err)
	target := filepath.Join(dir, "missing", "nested")
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	_, err = fslink.LinkDir(target, first)
	require.NoError(t, err)
	_, err = fslink.LinkDir(first, second)
	require.NoError(t, err)
	for _, path := range []string{target, first, second} {
		got, err := pathresolve.EvalSymlinksAllowMissing(filepath.Join(path, "config"))
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(canonical, "missing", "nested", "config"), got)
	}
	assert.NoDirExists(t, filepath.Join(dir, "missing"))
}

func TestEvalSymlinksAllowMissingRejectsInvalidPaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	got, err := pathresolve.EvalSymlinksAllowMissing(filepath.Join(file, "missing"))
	require.Error(t, err)
	assert.Empty(t, got)
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	_, err = fslink.LinkDir(second, first)
	require.NoError(t, err)
	_, err = fslink.LinkDir(first, second)
	require.NoError(t, err)
	for _, path := range []string{first, filepath.Join(first, "config")} {
		got, err = pathresolve.EvalSymlinksAllowMissing(path)
		require.Error(t, err)
		assert.Empty(t, got)
	}
}
