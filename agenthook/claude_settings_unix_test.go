//go:build unix

package agenthook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gittest "go.kenn.io/kit/git/test"
)

func TestCheckClaudeForeignOwnedLocalSettings(t *testing.T) {
	repo := gittest.NewRepoWithCommit(t)
	repo.WriteFile(".claude/settings.local.json", `{"disableAllHooks":false}`)
	rootSettings := filepath.Join(repo.Root, ".claude")
	uid := os.Geteuid()
	if err := os.Chown(rootSettings, uid+1, -1); os.IsPermission(err) {
		t.Skip("changing fixture ownership needs privileges")
	} else {
		require.NoError(t, err)
	}
	t.Cleanup(func() { require.NoError(t, os.Chown(rootSettings, uid, -1)) })
	workspace := filepath.Join(repo.Root, "subdir")
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, ".claude"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".claude", "settings.local.json"), []byte(`{"disableAllHooks":true}`), 0o600))
	t.Chdir(workspace)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	executable, err := os.Executable()
	require.NoError(t, err)
	opts := InstallOptions{Executable: executable, Arguments: []string{"example-marker"}, Marker: "example-marker", Hooks: []Hook{{Event: EventStop}}}
	_, err = Install(AgentClaude, opts)
	require.NoError(t, err)
	result, err := Check(t.Context(), AgentClaude, opts)
	require.NoError(t, err)
	assert.Equal(t, CheckDisabled, result.State, result.Reason)
}
