//go:build unix && !darwin && !linux

package safefileio_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func TestRestrictCurrentUserFileFailsClosedWhenUnsupported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() { _ = file.Close() }()

	err = safefileio.RestrictCurrentUserFile(file)
	require.ErrorContains(t, err, "current-user file restriction is unsupported")
}
