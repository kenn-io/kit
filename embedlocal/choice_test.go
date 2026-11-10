package embedlocal

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChoiceRecorded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".kenn", "embeddings")
	got, err := ChoiceRecorded()
	require.NoError(t, err)
	assert.False(t, got)
	_, err = os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, os.MkdirAll(dir, 0o700))
	got, err = ChoiceRecorded()
	require.NoError(t, err)
	assert.False(t, got)

	for _, tc := range []struct {
		name, contents string
		want           bool
	}{
		{"empty", "", false},
		{"partial", "yes", false},
		{"malformed", "no\n", false},
		{"crlf", "yes\r\n", false},
		{"unknown", "yes\nextra", false},
		{"chosen", "yes\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "search-by-meaning"), []byte(tc.contents), 0o600))
			got, err := ChoiceRecorded()
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRecordChoice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for range 2 {
		require.NoError(t, RecordChoice())
		data, err := os.ReadFile(filepath.Join(home, ".kenn", "embeddings", "search-by-meaning"))
		require.NoError(t, err)
		assert.Equal(t, "yes\n", string(data))
	}
}

func TestChoiceRecordedNonRegularMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".kenn", "embeddings", "search-by-meaning"), 0o700))
	got, err := ChoiceRecorded()
	require.Error(t, err)
	assert.False(t, got)
}
