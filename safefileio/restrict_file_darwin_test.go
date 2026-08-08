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

func TestRestrictCurrentUserFileRemovesExtendedACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
	output, err := exec.Command(
		"chmod",
		"+a",
		"everyone allow read",
		path,
	).CombinedOutput()
	require.NoError(t, err, string(output))
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() { _ = file.Close() }()

	require.NoError(t, safefileio.RestrictCurrentUserFile(file))
	listing, err := exec.Command("ls", "-lde", path).CombinedOutput()
	require.NoError(t, err, string(listing))
	assert.NotContains(t, string(listing), "everyone allow read")
}
