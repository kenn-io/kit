package embedserver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMissingRuntimePreventsModelDownload(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cache")
	_, err := loadModel(t.Context(), dir, filepath.Join(root, "missing-library"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestTextModelRequiresInstallation(t *testing.T) {
	root := t.TempDir()
	library := filepath.Join(root, "runtime")
	require.NoError(t, os.WriteFile(library, []byte("unused until models are installed"), 0o600))
	_, err := loadModel(t.Context(), filepath.Join(root, "models"), library)
	require.ErrorContains(t, err, "install nomic-embed-text: missing or corrupt model.onnx")
}
