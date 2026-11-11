//go:build darwin || linux

package safefileio

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestSecretOwnerModePolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		uid     uint32
		mode    fs.FileMode
		allowed bool
	}{
		{"owner read only", 1, 0o400, true},
		{"owner read write", 1, 0o600, true},
		{"owner no access", 1, 0, false},
		{"owner group readable", 1, 0o640, false},
		{"owner executable", 1, 0o700, false},
		{"root readable", 0, 0o444, true},
		{"root private", 0, 0o600, true},
		{"root group writable", 0, 0o620, false},
		{"root world writable", 0, 0o602, false},
		{"foreign private", 2, 0o600, false},
		{"directory", 1, os.ModeDir | 0o600, false},
		{"fifo", 0, os.ModeNamedPipe | 0o400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSecretInfo(ownedInfo{mode: tc.mode, uid: tc.uid}, 1)
			if tc.allowed {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestOpenSecretFileTraversesExecutableOnlyParentProperty(t *testing.T) {
	root := privateTempDir(t)
	parent := filepath.Join(root, "search-only")
	require.NoError(t, os.Mkdir(parent, 0o700))
	path := filepath.Join(parent, "token")
	want := []byte("secret")
	require.NoError(t, os.WriteFile(path, want, 0o600))
	defer func() { _ = os.Chmod(parent, 0o700) }()

	rapid.Check(t, func(ht *rapid.T) {
		mode := fs.FileMode(rapid.Uint32Range(0, 0o777).Draw(ht, "mode"))
		require.NoError(ht, os.Chmod(parent, mode))
		file, err := OpenSecretFile(path)
		if (os.Geteuid() != 0 && mode&0o100 == 0) || mode.Perm()&0o022 != 0 {
			require.Error(ht, err)
			assert.Nil(ht, file)
			return
		}
		require.NoError(ht, err)
		got, err := io.ReadAll(file)
		require.NoError(ht, err)
		assert.Equal(ht, want, got)
		require.NoError(ht, file.Close())
	})
}

func TestOpenSecretFilePreservesTraversedComponents(t *testing.T) {
	root := privateTempDir(t)
	blocked := filepath.Join(root, "blocked")
	require.NoError(t, os.Mkdir(blocked, 0o700))
	defer func() { _ = os.Chmod(blocked, 0o700) }()
	token := filepath.Join(root, "token")
	require.NoError(t, os.WriteFile(token, []byte("secret"), 0o600))
	path := blocked + string(filepath.Separator) + ".." + string(filepath.Separator) + "token"
	if os.Geteuid() != 0 {
		require.NoError(t, os.Chmod(blocked, 0o600))
		file, err := OpenSecretFile(path)
		require.Error(t, err)
		assert.Nil(t, file)
	}
	require.NoError(t, os.Chmod(blocked, 0o775))
	file, err := OpenSecretFile(path)
	require.ErrorIs(t, err, ErrWritableParent)
	assert.Nil(t, file)
}
