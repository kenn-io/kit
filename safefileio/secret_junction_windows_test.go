package safefileio_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/fslink"
	"go.kenn.io/kit/safefileio"
)

func TestSecretWindowsJunctionBoundary(t *testing.T) {
	dir := privateTempDir(t)
	target := filepath.Join(dir, "target")
	require.NoError(t, os.Mkdir(target, 0o700))
	path := filepath.Join(target, "token")
	file, err := safefileio.CreatePrivateFile(path)
	require.NoError(t, err)
	_, err = file.WriteString("secret")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	link := filepath.Join(dir, "junction")
	require.NoError(t, fslink.CreateJunction(target, link))
	file, err = safefileio.OpenSecretFile(link)
	require.Error(t, err)
	assert.Nil(t, file)
	// The retained policy checks the final entry, not ancestor reparse points.
	file, err = safefileio.OpenSecretFile(filepath.Join(link, "token"))
	require.NoError(t, err)
	defer func() { assert.NoError(t, file.Close()) }()
	data, err := io.ReadAll(file)
	require.NoError(t, err)
	assert.Equal(t, "secret", string(data))
}
