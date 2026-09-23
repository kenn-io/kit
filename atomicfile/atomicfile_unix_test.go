//go:build unix

package atomicfile_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/atomicfile"
)

func modeOf(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Mode().Perm()
}

// setUmask makes the test observe exact permissions rather than umask output.
func setUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

func TestWriteFilePermissions(t *testing.T) {
	setUmask(t, 0o077)
	tests := []struct {
		name     string
		existing fs.FileMode
		opts     []atomicfile.Option
		want     fs.FileMode
	}{
		{name: "default for new file", want: 0o600},
		{name: "WithPerm for new file", opts: []atomicfile.Option{atomicfile.WithPerm(0o644)}, want: 0o644},
		{name: "default replaces existing mode", existing: 0o640, want: 0o600},
		{name: "WithPerm replaces existing mode", existing: 0o640, opts: []atomicfile.Option{atomicfile.WithPerm(0o604)}, want: 0o604},
		{name: "WithPreserveMode keeps existing mode", existing: 0o640, opts: []atomicfile.Option{atomicfile.WithPreserveMode()}, want: 0o640},
		{
			name: "WithPreserveMode without existing file uses WithPerm",
			opts: []atomicfile.Option{atomicfile.WithPreserveMode(), atomicfile.WithPerm(0o640)},
			want: 0o640,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			target := filepath.Join(t.TempDir(), "target")
			if tt.existing != 0 {
				require.NoError(os.WriteFile(target, []byte("old"), 0o600))
				require.NoError(os.Chmod(target, tt.existing))
			}

			require.NoError(atomicfile.WriteFile(target, []byte("new"), tt.opts...))

			assert.Equal(t, tt.want, modeOf(t, target))
			assert.Equal(t, "new", readString(t, target))
		})
	}
}

func TestWriteNewAppliesPerm(t *testing.T) {
	setUmask(t, 0o077)
	target := filepath.Join(t.TempDir(), "target")

	require.NoError(t, atomicfile.WriteNew(target, []byte("data"), atomicfile.WithPerm(0o644)))

	assert.Equal(t, fs.FileMode(0o644), modeOf(t, target))
}

func TestSyncDirReportsMissingDirectory(t *testing.T) {
	err := atomicfile.SyncDir(filepath.Join(t.TempDir(), "missing"))

	require.ErrorIs(t, err, fs.ErrNotExist)
}
