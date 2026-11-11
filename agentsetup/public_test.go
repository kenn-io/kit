package agentsetup_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/agentsetup"
	"go.kenn.io/kit/internal/codexapp/testserver"
)

func TestApplicationSetup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	t.Setenv("SETUP_HELPER", "1")
	executable := testserver.InstallExecutable(t, "codex")
	custom := filepath.Join(home, "custom")
	opts := agentsetup.Options{SetupCommand: "example setup", Items: func(agent string) ([]agentsetup.Item, error) {
		return []agentsetup.Item{
			agentsetup.Hooks(agent, agenthook.InstallOptions{Executable: executable, Arguments: []string{"example-hook"}, Marker: "example-hook", Hooks: []agenthook.Hook{{Event: agenthook.EventStop}, {Event: agenthook.EventSessionStart}, {Event: agenthook.EventSessionEnd}}}),
			agentsetup.MCP(agent, "example", agentmcp.Server{Command: executable}),
			{Name: "custom", Install: func(context.Context) error { return os.WriteFile(custom, []byte("ready"), 0o600) }, Check: func(context.Context) (agentsetup.Status, error) {
				_, err := os.Stat(custom)
				if errors.Is(err, os.ErrNotExist) {
					return agentsetup.Status{State: "missing"}, nil
				}
				return agentsetup.Status{State: "ok"}, err
			}},
		}, nil
	}}
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		cmd := agentsetup.NewCommand(opts)
		cmd.SetOut(&out)
		cmd.SetArgs(append([]string{"--agent", "codex"}, args...))
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		err := cmd.ExecuteContext(t.Context())
		return out.String(), err
	}
	output, err := run()
	require.ErrorIs(t, err, agentsetup.ErrNotReady)
	require.Contains(t, output, "needs you")
	require.Contains(t, output, "/hooks")
	paths := []string{filepath.Join(home, "hooks.json"), filepath.Join(home, "config.toml"), custom}
	before := make([][]byte, len(paths))
	for i, path := range paths {
		before[i], err = os.ReadFile(path)
		require.NoError(t, err)
	}
	_, err = run()
	require.ErrorIs(t, err, agentsetup.ErrNotReady)
	output, err = run("--check", "--json")
	require.ErrorIs(t, err, agentsetup.ErrNotReady)
	var report agentsetup.Report
	require.NoError(t, json.Unmarshal([]byte(output), &report))
	require.Len(t, report.Rows, 3)
	require.Equal(t, agentsetup.StateUntrusted, report.Rows[0].State)
	require.Equal(t, 3, report.Rows[0].HookCount)
	require.Equal(t, agentsetup.StateOK, report.Rows[1].State)
	require.Equal(t, agentsetup.StateOK, report.Rows[2].State)
	output, err = run("--print")
	require.NoError(t, err)
	require.Contains(t, output, "codex")
	t.Setenv("SETUP_TRUST", "trusted")
	output, err = run("--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(output), &report))
	require.Equal(t, new(1), report.Rows[1].ToolCount)
	for i, path := range paths {
		after, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, before[i], after)
	}
}
