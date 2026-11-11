//go:build windows

package secretref_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
	"go.kenn.io/kit/secretref"
)

func TestWindowsRootedAndDriveRelativeFiles(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "token")
	f, err := safefileio.CreatePrivateFile(path)
	require.NoError(t, err)
	_, err = f.WriteString("secret")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	for _, selector := range []string{path[len(filepath.VolumeName(path)):], filepath.VolumeName(path) + "token"} {
		t.Run(selector, func(t *testing.T) {
			got, err := (secretref.Ref{File: selector}).Resolve()
			require.NoError(t, err)
			require.Equal(t, "secret", got.Value)
		})
	}
}

func TestWindowsRootedFileReachesFilePolicy(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(path, []byte("secret"), 0o600))
	_, err := (secretref.Ref{File: path[len(filepath.VolumeName(path)):]}).Resolve()
	if err != nil {
		var source *secretref.SourceError
		require.ErrorAs(t, err, &source)
		require.NotEqual(t, "file is missing", source.Reason)
	}
}
