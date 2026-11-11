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

func TestHomeAndAbsolutePaths(t *testing.T) {
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
	for _, tc := range []struct {
		suffix   string
		succeeds bool
	}{
		{"link/../token", true}, {"target//./token", true},
		{"target/inner/../token", true}, {"target/token/", false},
		{"missing/../target/token", false},
	} {
		for _, prefix := range []string{home + "/", "~/"} {
			got, err := (secretref.Ref{File: prefix + tc.suffix}).Resolve()
			if tc.succeeds {
				require.NoError(t, err, tc.suffix)
				assert.Equal(t, "fixed", got.Value)
			} else {
				require.Error(t, err, tc.suffix)
			}
		}
	}
}

func TestWritableParentReason(t *testing.T) {
	dir := privateTempDir(t)
	unsafe := filepath.Join(dir, "unsafe")
	require.NoError(t, os.Mkdir(unsafe, 0o700))
	key := filepath.Join(unsafe, "token")
	writePrivateFile(t, key, "secret")
	require.NoError(t, os.Chmod(unsafe, 0o775))
	_, err := (secretref.Ref{File: key}).Resolve()
	var source *secretref.SourceError
	require.ErrorAs(t, err, &source)
	require.Equal(t, "parent directory is writable by group or others", source.Reason)
}
