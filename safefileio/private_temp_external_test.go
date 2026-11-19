package safefileio_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	return dir
}
