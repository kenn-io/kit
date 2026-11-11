package agentsetup

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/agentmcp"
)

func TestDupAgents(t *testing.T) {
	installs, resolved := 0, 0
	report, err := Run(t.Context(), Options{Agents: []string{"claude", "Claude"}, SetupCommand: "app setup", Items: func(string) ([]Item, error) {
		resolved++
		return []Item{{Name: "item", Install: func(context.Context) error { installs++; return nil }, Check: func(context.Context) (Status, error) { return Status{State: StateMissing}, nil }}}, nil
	}})
	require.NoError(t, err)
	require.Len(t, report.Rows, 1)
	require.Equal(t, 1, resolved)
	require.Equal(t, 1, installs)
	require.Equal(t, "app setup --agent claude", report.Rows[0].NextStep)
}

func TestReviewFailedHookUpdateShowsUsableRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	hook := Hooks("claude", agenthook.InstallOptions{Executable: "example", Arguments: []string{"example-hook"}, Marker: "example-hook", Hooks: []agenthook.Hook{{Event: agenthook.EventStop}}})
	hook.Install = func(context.Context) error { return errors.New("write failed") }
	report, err := Run(t.Context(), Options{Agents: []string{"claude"}, SetupCommand: "app setup", Items: func(string) ([]Item, error) { return []Item{hook}, nil }})
	require.NoError(t, err)
	require.Equal(t, StateFailed, report.Rows[0].State)
	require.Equal(t, "write failed", report.Rows[0].Reason)
	require.Equal(t, "app setup --agent claude", report.Rows[0].NextStep)
}

func TestPrintUnsupportedAgent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var out bytes.Buffer
	err := Print(&out, Options{Agents: []string{"claude", "cursor"}, Items: func(agent string) ([]Item, error) {
		return []Item{MCP(agent, "example", agentmcp.Server{Command: "example"})}, nil
	}})
	require.Error(t, err)
	require.Empty(t, out.String())
	err = Print(&out, Options{Items: func(agent string) ([]Item, error) {
		return []Item{MCP(agent, "example", agentmcp.Server{Command: "example"})}, nil
	}})
	require.NoError(t, err)
	require.Contains(t, out.String(), "claude mcp add")
	require.Contains(t, out.String(), "codex mcp add")
}

func TestMissingAgentMCPGuidance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	server := agentmcp.Server{Command: "example"}
	_, err := agentmcp.Install(agentmcp.AgentClaude, agentmcp.InstallOptions{Name: "example", Server: server})
	require.NoError(t, err)
	report, err := Run(t.Context(), Options{Agents: []string{"claude"}, SetupCommand: "app setup", CheckOnly: true, Items: func(agent string) ([]Item, error) { return []Item{MCP(agent, "example", server)}, nil }})
	require.NoError(t, err)
	require.Equal(t, StateUnknown, report.Rows[0].State)
	require.Contains(t, report.Rows[0].NextStep, "Install Claude Code")
	require.Contains(t, report.Rows[0].NextStep, "app setup --agent claude --check")
}

func TestDesignSummary(t *testing.T) {
	r := Report{Rows: make([]Row, 2)}
	r.Rows[0].State, r.Rows[1].State = StateMissing, StateFailed
	var out bytes.Buffer
	require.NoError(t, r.WriteText(&out, "app setup --check"))
	require.Contains(t, out.String(), "Done except 2 steps for you. Run `app setup --check` afterwards.")
}
