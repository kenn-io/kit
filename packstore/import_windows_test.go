//go:build windows

package packstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
)

func TestPrepareImportWindowsPublishesWithClosedHandlesAndReopens(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))

	prepared, err := PrepareImport(context.Background(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

	require.NoError(err)
	assert.Equal([]Hash{hashFromEntry(t, entries[0])}, prepared.PackedHashes())
	finalName := importPackPath("content", packID)
	renamedName := finalName + ".renamed"
	require.NoError(target.Rename(finalName, renamedName))
	final, err := target.Open(renamedName)
	require.NoError(err)
	assert.NoError(final.Close())
	require.NoError(target.Rename(renamedName, finalName))
}

func TestPrepareImportWindowsReusesByteIdenticalDestination(t *testing.T) {
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
	opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
	requirePrepared, err := PrepareImport(context.Background(), target, "content", input, opts)
	Require.NoError(t, err)

	reused, err := PrepareImport(context.Background(), target, "content", input, opts)

	Require.NoError(t, err)
	Assert.Equal(t, requirePrepared.PackedHashes(), reused.PackedHashes())
}

func TestPrepareImportWindowsRefusesCollisionWithoutReplacing(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
	opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
	_, err := PrepareImport(context.Background(), target, "content", input, opts)
	require.NoError(err)
	final := filepath.Join(target.Name(), filepath.FromSlash(importPackPath("content", packID)))
	require.NoError(os.WriteFile(final, []byte("collision"), 0o600))

	_, err = PrepareImport(context.Background(), target, "content", input, opts)

	assert.ErrorContains(err, "collision")
	data, readErr := os.ReadFile(final)
	require.NoError(readErr)
	assert.Equal([]byte("collision"), data)
}
