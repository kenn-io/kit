//go:build unix

package packstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestFilesystemBackendRetireRejectsSymlinkedLooseShard(t *testing.T) {
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	content := []byte("outside loose object")
	hash := hashForTest(content)
	external := t.TempDir()
	externalPath := filepath.Join(external, hash.String())
	require.NoError(t, os.WriteFile(externalPath, content, 0o600))
	require.NoError(t, os.Symlink(external, filepath.Join(backend.Layout().Root(), hash.String()[:2])))

	err := backend.Retire(context.Background(), ObjectRef{
		LooseHash: hash, LooseEncoding: LooseEncodingRaw,
	})

	require.ErrorContains(t, err, "unsafe filesystem directory")
	got, readErr := os.ReadFile(externalPath)
	require.NoError(t, readErr)
	assert.Equal(t, content, got)
}

func TestFilesystemBackendRetireRejectsSymlinkedPackShard(t *testing.T) {
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	packID := pack.NewPackID()
	external := t.TempDir()
	externalPath := filepath.Join(external, packID+PackExt)
	require.NoError(t, os.WriteFile(externalPath, []byte("outside pack"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(backend.Layout().Root(), "packs"), 0o700))
	require.NoError(t, os.Symlink(external, filepath.Join(
		backend.Layout().Root(), "packs", packID[:2],
	)))

	err := backend.Retire(context.Background(), ObjectRef{PackID: packID})

	require.ErrorContains(t, err, "unsafe filesystem directory")
	got, readErr := os.ReadFile(externalPath)
	require.NoError(t, readErr)
	assert.Equal(t, []byte("outside pack"), got)
}

func TestFilesystemBackendPublishPackRejectsSymlinkedPackDirectory(t *testing.T) {
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	external := t.TempDir()
	require.NoError(t, os.Symlink(external, filepath.Join(backend.Layout().Root(), "packs")))
	path, packID, _ := buildBackendPackSource(t, []byte("confined pack publication"))
	source, err := os.Open(path)
	require.NoError(t, err)

	_, err = backend.PublishPack(context.Background(), packID, source, PublishOptions{})
	require.NoError(t, source.Close())

	require.ErrorContains(t, err, "unsafe filesystem directory")
	entries, readErr := os.ReadDir(external)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}
