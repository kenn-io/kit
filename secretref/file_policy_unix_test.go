//go:build darwin || linux

package secretref_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/secretref"
)

func TestMountedAndHomeSymlinkPaths(t *testing.T) {
	home := privateTempDir(t)
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "target", "inner"), 0o700))
	require.NoError(t, os.Symlink("target/inner", filepath.Join(home, "link")))
	version := filepath.Join(home, "..version")
	require.NoError(t, os.Mkdir(version, 0o700))
	writePrivateFile(t, filepath.Join(version, "token"), "mounted\r\n")
	require.NoError(t, os.Chmod(filepath.Join(version, "token"), 0o400))
	require.NoError(t, os.Symlink("..version", filepath.Join(home, "..data")))
	require.NoError(t, os.Symlink("..data/token", filepath.Join(home, "mounted")))
	mounted, err := (secretref.Ref{File: "~/mounted"}).Resolve()
	require.NoError(t, err)
	assert.Equal(t, "mounted", mounted.Value)
	writePrivateFile(t, filepath.Join(home, "target", "token"), "fixed")
	got, err := (secretref.Ref{File: "~/link/../token"}).Resolve()
	require.NoError(t, err)
	assert.Equal(t, "fixed", got.Value)
}
