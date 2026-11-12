package agentsetup

import (
	"bytes"
	"context"
	"runtime"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/agentmcp"
)

func execute(ctx context.Context, opts Options, args ...string) (string, error) {
	var out bytes.Buffer
	root := &cobra.Command{Use: "app", SilenceErrors: true, SilenceUsage: true}
	root.SetOut(&out)
	root.AddCommand(NewCommand(opts))
	root.SetArgs(append([]string{"setup"}, args...))
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

func TestCommandCheckAndPrint(t *testing.T) {
	installed := false
	opts := Options{Items: func(agent string) ([]Item, error) {
		return []Item{{Name: "custom", Install: func(context.Context) error { installed = true; return nil }, Check: func(context.Context) (Status, error) { return Status{State: StateMissing}, nil }}, MCP(agent, "example", agentmcp.Server{Command: "example"})}, nil
	}}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	text, err := execute(t.Context(), opts, "--agent", "claude", "--check")
	require.ErrorIs(t, err, ErrNotReady)
	require.False(t, installed)
	require.Contains(t, text, "app setup --agent claude")
	output, err := execute(t.Context(), opts, "--print", "--agent", "codex")
	require.NoError(t, err)
	if runtime.GOOS == "windows" {
		require.Contains(t, output, "'codex' 'mcp' 'add'")
	} else {
		require.Contains(t, output, "codex mcp add")
	}
	require.False(t, installed)
}

func TestCommandReportsCancellation(t *testing.T) {
	for _, args := range [][]string{{"--agent", "codex"}, {"--agent", "codex", "--json"}} {
		ctx, cancel := context.WithCancel(t.Context())
		out, err := execute(ctx, Options{Items: func(string) ([]Item, error) {
			return []Item{{Name: "interrupted", Check: func(context.Context) (Status, error) { cancel(); return Status{State: StateOK}, nil }}}, nil
		}}, args...)
		require.ErrorIs(t, err, context.Canceled)
		require.Contains(t, out, "interrupted")
		require.Contains(t, out, "unknown")
		cancel()
	}
}

func TestCommandCancellationWithNoItems(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out, err := execute(ctx, Options{Items: func(string) ([]Item, error) {
		cancel()
		return nil, nil
	}}, "--agent", "codex")
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, out)
}

func TestCommandRepairGuidance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	opts := Options{RepairCommand: "app agent-hook install", Items: func(agent string) ([]Item, error) {
		return []Item{MCP(agent, "example", agentmcp.Server{Command: "example"})}, nil
	}}
	out, err := execute(t.Context(), opts, "--agent", "codex", "--check")
	require.ErrorIs(t, err, ErrNotReady)
	require.Contains(t, out, "The named MCP server is missing: app agent-hook install")
	require.Contains(t, out, "Run `app setup --agent codex --check` afterwards.")
	out, err = execute(t.Context(), opts, "--agent", "codex", "--check", "--json")
	require.ErrorIs(t, err, ErrNotReady)
	require.Contains(t, out, `"next_step":"app agent-hook install"`)
	opts.RepairCommand = ""
	out, err = execute(t.Context(), opts, "--agent", "codex", "--check", "--json")
	require.ErrorIs(t, err, ErrNotReady)
	require.Contains(t, out, `"next_step":"app setup --agent codex"`)
}

func TestCommandUnknownAgentReport(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	opts := Options{Items: func(agent string) ([]Item, error) {
		return []Item{Hooks(agent, agenthook.InstallOptions{Executable: "example", Marker: "example", Hooks: []agenthook.Hook{{Event: agenthook.EventStop}}}), MCP(agent, "example", agentmcp.Server{Command: "example"})}, nil
	}}
	out, err := execute(t.Context(), opts, "--agent", "future,claude", "--check")
	require.ErrorIs(t, err, ErrNotReady)
	require.Contains(t, out, "future")
	require.Contains(t, out, "unknown")
	require.Contains(t, out, "Claude Code")
	out, err = execute(t.Context(), opts, "--agent", "future,claude", "--check", "--json")
	require.ErrorIs(t, err, ErrNotReady)
	require.Contains(t, out, `"agent":"future","agent_name":"future","piece":"hooks","state":"unknown"`)
	require.Contains(t, out, `"agent":"future","agent_name":"future","piece":"mcp example","state":"unknown"`)
}

func TestCommandOutdatedRepairGuidance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	_, err := agentmcp.Install(agentmcp.AgentClaude, agentmcp.InstallOptions{Name: "example", Server: agentmcp.Server{Command: "old"}})
	require.NoError(t, err)
	opts := Options{RepairCommand: "app repair", Items: func(agent string) ([]Item, error) {
		return []Item{MCP(agent, "example", agentmcp.Server{Command: "new"})}, nil
	}}
	out, err := execute(t.Context(), opts, "--agent", "claude", "--check")
	require.ErrorIs(t, err, ErrNotReady)
	require.Contains(t, out, "outdated")
	require.Contains(t, out, "app repair")
	require.Contains(t, out, "Run `app setup --agent claude --check` afterwards.")
}
