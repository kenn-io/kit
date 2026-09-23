//go:build !windows

package humacheck

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/fslink"
)

const jsonV1Source = "package m\n\nimport \"encoding/json\"\n\nvar _ = json.Marshal\n"

func TestRewriteJSONV1FileKeepsMode(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "m.go")
	require.NoError(os.WriteFile(path, []byte(jsonV1Source), 0o600))
	require.NoError(os.Chmod(path, 0o751))

	changed, err := rewriteJSONV1File(path)

	require.NoError(err)
	assert.True(changed)
	info, err := os.Stat(path)
	require.NoError(err)
	assert.Equal(os.FileMode(0o751), info.Mode().Perm())
	content, err := os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(content), `"encoding/json/v2"`)
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(err)
	assert.Len(entries, 1, "no staging file is left behind")
}

func TestRewriteJSONV1FileRefusesSymlink(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target.go")
	link := filepath.Join(dir, "m.go")
	require.NoError(os.WriteFile(target, []byte(jsonV1Source), 0o644))
	require.NoError(os.Symlink(target, link))

	changed, err := rewriteJSONV1File(link)

	require.ErrorIs(err, fslink.ErrIsLink)
	assert.False(changed)
	info, err := os.Lstat(link)
	require.NoError(err)
	assert.NotZero(info.Mode() & os.ModeSymlink)
	content, err := os.ReadFile(target)
	require.NoError(err)
	assert.Equal(jsonV1Source, string(content))
}
