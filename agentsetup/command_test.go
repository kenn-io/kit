package agentsetup

import (
	"bytes"
	"context"
	"encoding/json/v2"
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
	data, err := run("--check", "--json")
	require.ErrorIs(t, err, ErrNotReady)
	var report Report
	require.NoError(t, json.Unmarshal([]byte(data), &report))
	require.Equal(t, "app setup --agent claude", report.Rows[0].NextStep)
	require.Contains(t, text, report.Rows[0].NextStep)
	output, err := run("--print")
	require.NoError(t, err)
	require.Contains(t, output, "claude mcp add")
	require.False(t, installed)
}
