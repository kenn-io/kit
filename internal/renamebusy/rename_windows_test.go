package renamebusy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestReplaceStopsBeforeDelayExceedsBudget(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	dst := filepath.Join(dir, "destination")
	require.NoError(t, os.WriteFile(src, []byte("original"), 0o600))
	handle, err := os.Open(src)
	require.NoError(t, err)
	defer func() { assert.NoError(t, handle.Close()) }()

	started := time.Now()
	err = Replace(src, dst, 20*time.Millisecond)
	elapsed := time.Since(started)
	require.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)
	var renameErr *os.LinkError
	require.ErrorAs(t, err, &renameErr)
	assert.Equal(t, src, renameErr.Old)
	assert.Equal(t, dst, renameErr.New)
	assert.Less(t, elapsed, 500*time.Millisecond)
	data, err := os.ReadFile(src)
	require.NoError(t, err)
	assert.Equal(t, "original", string(data))
	assert.NoFileExists(t, dst)
}

func TestReplaceRetriesSharingViolation(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	dst := filepath.Join(dir, "destination")
	require.NoError(t, os.WriteFile(src, []byte("original"), 0o600))
	handle, err := os.Open(src)
	require.NoError(t, err)
	closed := make(chan error, 1)
	time.AfterFunc(100*time.Millisecond, func() { closed <- handle.Close() }) //nolint:kennlint // exercise a real Windows sharing violation until the reader closes

	require.NoError(t, Replace(src, dst, DefaultTimeout))
	require.NoError(t, <-closed)
	data, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "original", string(data))
	assert.NoFileExists(t, src)
}
