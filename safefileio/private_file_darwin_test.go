package safefileio_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func TestValidatePrivateCurrentUserFileRejectsExtendedACL(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "record.json")
	require.NoError(os.WriteFile(path, []byte("{}"), 0o600))
	output, err := exec.CommandContext(t.Context(),
		"chmod",
		"+a",
		"everyone allow read",
		path,
	).CombinedOutput()
	require.NoError(err, string(output))
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	defer func() { _ = file.Close() }()

	require.Error(safefileio.ValidatePrivateCurrentUserFile(file))
	listing, err := exec.CommandContext(t.Context(), "ls", "-lde", path).CombinedOutput()
	require.NoError(err, string(listing))
	assert.Contains(t, string(listing), "everyone allow read")
}
