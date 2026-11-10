package embedlocal

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/fslink"
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

func TestChoiceAcrossProcesses(t *testing.T) {
	if os.Getenv("KIT_CHOICE_CHILD") == "1" {
		got, err := ChoiceRecorded()
		require.NoError(t, err)
		require.True(t, got)
		return
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	require.NoError(t, RecordChoice())
	exe, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestChoiceAcrossProcesses$")
	cmd.Env = append(os.Environ(), "KIT_CHOICE_CHILD=1")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestRecordChoiceThroughLinkedDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	target := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".kenn"), 0o700))
	_, err := fslink.LinkDir(target, filepath.Join(home, ".kenn", "embeddings"))
	require.NoError(t, err)
	require.NoError(t, RecordChoice())
	data, err := os.ReadFile(filepath.Join(target, "search-by-meaning"))
	require.NoError(t, err)
	assert.Equal(t, "yes\n", string(data))
	got, err := ChoiceRecorded()
	require.NoError(t, err)
	assert.True(t, got)
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
