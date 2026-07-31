package s3store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func TestCreatePrivateTempUsesValidatedUserDirectory(t *testing.T) {
	base := t.TempDir()
	file, err := createPrivateTempIn(base, "stage-*")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, file.Close())
		require.NoError(t, os.Remove(file.Name()))
	})

	dir := filepath.Dir(file.Name())
	require.NoError(t, safefileio.ValidatePrivateDir(dir))
	assert.Equal(t, base, filepath.Dir(dir))
}
