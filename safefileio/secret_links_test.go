//go:build darwin || linux

package safefileio_test

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func TestSecretLinks(t *testing.T) {
	// Canonicalize only the fixture prefix, so /var's own link does not consume
	// one of the deliberately generated chain's 40 hops on macOS.
	dir, err := filepath.EvalSymlinks(privateTempDir(t))
	require.NoError(t, err)
	target := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o600))
	version := filepath.Join(dir, "..version")
	require.NoError(t, os.Mkdir(version, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(version, "token"), []byte("mounted"), 0o400))
	require.NoError(t, os.Symlink("..version", filepath.Join(dir, "..data")))
	require.NoError(t, os.Symlink("..data/token", filepath.Join(dir, "mounted")))
	mounted, err := safefileio.OpenSecretFile(filepath.Join(dir, "mounted"))
	require.NoError(t, err)
	data, err := io.ReadAll(mounted)
	require.NoError(t, err)
	assert.Equal(t, "mounted", string(data))
	require.NoError(t, mounted.Close())
	paths := []string{target}
	for i := 1; i <= 41; i++ {
		path := filepath.Join(dir, strconv.Itoa(i))
		require.NoError(t, os.Symlink(filepath.Base(paths[i-1]), path))
		paths = append(paths, path)
	}
	file, err := safefileio.OpenSecretFile(paths[40])
	require.NoError(t, err)
	require.NoError(t, file.Close())
	file, err = safefileio.OpenSecretFile(paths[41])
	require.Error(t, err)
	assert.Nil(t, file)
	loop := filepath.Join(dir, "loop")
	require.NoError(t, os.Symlink("loop", loop))
	file, err = safefileio.OpenSecretFile(loop)
	require.Error(t, err)
	assert.Nil(t, file)
}
