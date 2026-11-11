package agentsetup

import (
	"bytes"
	"context"
	"os"
	"runtime"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentmcp"
)

func TestCommandCheckAndPrint(t *testing.T) {
	installed := false
	opts := Options{Items: func(agent string) ([]Item, error) {
		return []Item{{Name: "custom", Install: func(context.Context) error { installed = true; return nil }, Check: func(context.Context) (Status, error) { return Status{State: StateMissing}, nil }}, MCP(agent, "example", agentmcp.Server{Command: "example"})}, nil
	}}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		root := &cobra.Command{Use: "app", SilenceErrors: true, SilenceUsage: true}
		root.SetOut(&out)
		root.AddCommand(NewCommand(opts))
		root.SetArgs(append([]string{"setup", "--agent", "claude"}, args...))
		err := root.ExecuteContext(t.Context())
		return out.String(), err
	}
	text, err := run("--check")
	require.ErrorIs(t, err, ErrNotReady)
	require.False(t, installed)
	require.Contains(t, text, "app setup --agent claude")
	output, err := run("--print", "--agent", "codex")
	require.NoError(t, err)
	if runtime.GOOS == "windows" {
		require.Contains(t, output, "'codex' 'mcp' 'add'")
	} else {
		require.Contains(t, output, "codex mcp add")
	}
	require.False(t, installed)
}

func TestCommandInstallFailureGuidance(t *testing.T) {
	for _, tc := range []struct {
		name, agent, reason, step string
	}{
		{"malformed config", "claude", "mcpServers must be an object", "Correct the agent MCP config"},
		{"unsupported agent", "muse", `unsupported MCP agent "muse"`, "Choose an agent that supports MCP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			if tc.agent == "claude" {
				path, err := agentmcp.ConfigPath(agentmcp.AgentClaude)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, []byte(`{"mcpServers":null}`), 0o600))
			}
			var out bytes.Buffer
			root := &cobra.Command{Use: "app", SilenceErrors: true, SilenceUsage: true}
			root.SetOut(&out)
			root.AddCommand(NewCommand(Options{Items: func(agent string) ([]Item, error) {
				return []Item{MCP(agent, "example", agentmcp.Server{Command: "example"})}, nil
			}}))
			root.SetArgs([]string{"setup", "--agent", tc.agent})
			require.ErrorIs(t, root.ExecuteContext(t.Context()), ErrNotReady)
			require.Contains(t, out.String(), tc.reason)
			require.Contains(t, out.String(), tc.step)
		})
	}
}
