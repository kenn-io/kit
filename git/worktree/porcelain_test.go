package gitworktree

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePorcelain(t *testing.T) {
	output := "" +
		"worktree /repo\nHEAD abc123\nbranch refs/heads/main\n\n" +
		"worktree /repo/.wt/feature\nHEAD def456\nbranch refs/heads/feature/x\n\n" +
		"worktree /bare\nbare\n\n" +
		"worktree /repo/.wt/detached\nHEAD 9f9f9f9f9f9f9f\ndetached\n" +
		"prunable gitdir file points to non-existent location\n"

	entries := ParsePorcelain(output)
	require.Len(t, entries, 4)

	assert.Equal(t, PorcelainEntry{Path: "/repo", Head: "abc123", Branch: "main"}, entries[0])
	assert.Equal(t, PorcelainEntry{Path: "/repo/.wt/feature", Head: "def456", Branch: "feature/x"}, entries[1])
	assert.Equal(t, PorcelainEntry{Path: "/bare", Bare: true}, entries[2])
	assert.Equal(t, PorcelainEntry{
		Path: "/repo/.wt/detached", Head: "9f9f9f9f9f9f9f", Detached: true,
		Prunable: true, PrunableReason: "gitdir file points to non-existent location",
	}, entries[3])
}

func TestParsePorcelainIgnoresUnknownFieldsAndIncompleteBlocks(t *testing.T) {
	entries := ParsePorcelain("HEAD ignored\n\nworktree /repo\nlocked reason\ncustom value\n")

	require.Len(t, entries, 1)
	assert.Equal(t, "/repo", entries[0].Path)
	assert.True(t, entries[0].Locked)
	assert.Equal(t, "reason", entries[0].LockedReason)
}
