//go:build windows

package packstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareImportWindowsPublishesWithClosedHandlesAndReopens(t *testing.T) {
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))

	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

	require.NoError(t, err)
	assert.Equal(t, []Hash{hashFromEntry(t, entries[0])}, prepared.PackedHashes())
	final, err := target.Open(importPackPath("content", packID))
	require.NoError(t, err)
	assert.NoError(t, final.Close())
}

func TestPrepareImportWindowsReusesByteIdenticalDestination(t *testing.T) {
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
	opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
	requirePrepared, err := PrepareImport(context.Background(), target, "content", input, opts)
	require.NoError(t, err)

	reused, err := PrepareImport(context.Background(), target, "content", input, opts)

	require.NoError(t, err)
	assert.Equal(t, requirePrepared.PackedHashes(), reused.PackedHashes())
}

func TestPrepareImportWindowsRefusesCollisionWithoutReplacing(t *testing.T) {
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
	opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
	_, err := PrepareImport(context.Background(), target, "content", input, opts)
	require.NoError(t, err)
	final := filepath.Join(target.Name(), filepath.FromSlash(importPackPath("content", packID)))
	require.NoError(t, os.WriteFile(final, []byte("collision"), 0o600))

	_, err = PrepareImport(context.Background(), target, "content", input, opts)

	assert.ErrorContains(t, err, "collision")
	data, readErr := os.ReadFile(final)
	require.NoError(t, readErr)
	assert.Equal(t, []byte("collision"), data)
}
