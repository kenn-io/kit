//go:build windows

package secretref_test

import (
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
