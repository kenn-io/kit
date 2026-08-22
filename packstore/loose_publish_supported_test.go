//go:build darwin || linux || windows

package packstore

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
)

func TestLoosePublicationFallbackNeverReplacesExistingDestination(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	stagedContent := []byte("new staged content")
	existingContent := []byte("existing canonical content")
	require.NoError(os.WriteFile(staging, stagedContent, 0o600))
	require.NoError(os.WriteFile(final, existingContent, 0o600))
	originalLink := linkLoosePublicationFile
	linkLoosePublicationFile = func(string, string) error { return fs.ErrInvalid }
	t.Cleanup(func() { linkLoosePublicationFile = originalLink })

	err := publishLooseFileNoReplace(staging, final)

	require.ErrorIs(err, fs.ErrExist)
	assert.Equal(stagedContent, mustReadFile(t, staging))
	assert.Equal(existingContent, mustReadFile(t, final))
}
