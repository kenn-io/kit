//go:build windows

package packstore

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplaceLooseRepairFileWindowsReplacesActiveReader(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	before := []byte("old open descriptor")
	after := []byte("verified replacement")
	require.NoError(os.WriteFile(staging, after, 0o600))
	require.NoError(os.WriteFile(final, before, 0o600))
	active, err := openNoFollow(final, false)
	require.NoError(err)
	t.Cleanup(func() { _ = active.Close() })

	err = replaceLooseRepairFile(staging, final)

	require.NoError(err)
	assert.Equal(after, mustReadFile(t, final))
	oldBytes, err := io.ReadAll(active)
	require.NoError(err)
	assert.Equal(before, oldBytes)
}

func TestReplaceLooseRepairFileWindowsCreatesAbsentTargetWithoutClobber(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	final := filepath.Join(dir, "final")
	content := []byte("verified absent-target repair")
	require.NoError(os.WriteFile(staging, content, 0o600))

	err := replaceLooseRepairFile(staging, final)

	require.NoError(err)
	assert.Equal(content, mustReadFile(t, final))
}

func TestReplaceLooseRepairFileWindowsRetriesReplaceAfterCreateRace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	missing := &fs.PathError{Op: "ReplaceFileW", Path: "final", Err: fs.ErrNotExist}
	originalReplace := replaceLooseFileWindows
	originalLink := linkLooseRepairFileWindows
	var calls []string
	replaceLooseFileWindows = func(staging, final string) error {
		calls = append(calls, "replace:"+staging+":"+final)
		if len(calls) == 1 {
			return missing
		}
		return nil
	}
	linkLooseRepairFileWindows = func(staging, final string) error {
		calls = append(calls, "link:"+staging+":"+final)
		return fs.ErrExist
	}
	t.Cleanup(func() {
		replaceLooseFileWindows = originalReplace
		linkLooseRepairFileWindows = originalLink
	})

	err := replaceLooseRepairFile("staging", "final")

	require.NoError(err)
	assert.Equal([]string{
		"replace:staging:final",
		"link:staging:final",
		"replace:staging:final",
	}, calls)
}
