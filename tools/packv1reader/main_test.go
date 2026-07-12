package main

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestReadFixture(t *testing.T) {
	dir := t.TempDir()
	writer, err := pack.NewWriter(dir, pack.WriterOptions{})
	require.NoError(t, err)
	_, err = writer.Append([]byte("raw"))
	require.NoError(t, err)
	_, err = writer.Append(bytes.Repeat([]byte("compressed"), 4096))
	require.NoError(t, err)
	path := filepath.Join(dir, "fixture.pack")
	_, err = writer.Seal(path)
	require.NoError(t, err)
	require.NoError(t, readFixture(path))
}
