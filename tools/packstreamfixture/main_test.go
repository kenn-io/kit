package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestWriteFixtureContainsRawAndCompressedEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stream-v1.pack")
	require.NoError(t, writeFixture(path))
	reader, err := pack.OpenReader(path, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	entries := reader.Entries()
	require.Len(t, entries, 2)
	assert.Zero(t, entries[0].Flags&pack.BlobCompressed)
	assert.NotZero(t, entries[1].Flags&pack.BlobCompressed)
}
