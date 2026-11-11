package agentsetup

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/internal/codexapp/testserver"
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

func TestMissingAgentMCPGuidance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	server := agentmcp.Server{Command: "example"}
	_, err := agentmcp.Install(agentmcp.AgentClaude, agentmcp.InstallOptions{Name: "example", Server: server})
	require.NoError(t, err)
	report, err := Run(t.Context(), Options{Agents: []string{"claude"}, SetupCommand: "app setup", CheckOnly: true, Items: func(agent string) ([]Item, error) { return []Item{MCP(agent, "example", server)}, nil }})
	require.NoError(t, err)
	require.Equal(t, StateUnknown, report.Rows[0].State)
	require.Contains(t, report.Rows[0].NextStep, "Install Claude Code")
	var out bytes.Buffer
	require.NoError(t, report.WriteText(&out, "app setup --agent claude --check"))
	require.Equal(t, 1, strings.Count(out.String(), "app setup --agent claude --check"))
}

func TestPrint(t *testing.T) {
	t.Run("unsupported", func(t *testing.T) {
		var out bytes.Buffer
		err := Print(&out, Options{Agents: []string{"claude", "cursor"}, Items: func(agent string) ([]Item, error) {
			return []Item{MCP(agent, "example", agentmcp.Server{Command: "example"})}, nil
		}})
		require.Error(t, err)
		require.Contains(t, out.String(), "claude")
	})
	t.Run("defaults without installed agents", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		var out bytes.Buffer
		err := Print(&out, Options{Items: func(agent string) ([]Item, error) {
			return []Item{MCP(agent, "example", agentmcp.Server{Command: "example"})}, nil
		}})
		require.NoError(t, err)
		if runtime.GOOS == "windows" {
			require.Contains(t, out.String(), "'claude' 'mcp' 'add'")
			require.Contains(t, out.String(), "'codex' 'mcp' 'add'")
		} else {
			require.Contains(t, out.String(), "claude mcp add")
			require.Contains(t, out.String(), "codex mcp add")
		}
	})
	t.Run("defaults to installed agent", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		testserver.InstallExecutable(t, "codex")
		var out bytes.Buffer
		err := Print(&out, Options{Items: func(agent string) ([]Item, error) {
			return []Item{MCP(agent, "example", agentmcp.Server{Command: "example"})}, nil
		}})
		require.NoError(t, err)
		require.Contains(t, out.String(), "codex")
		require.NotContains(t, out.String(), "claude")
	})
}

func TestCodexZeroToolsInReport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("SETUP_HELPER", "1")
	testserver.InstallExecutable(t, "codex")
	testserver.InstallExecutable(t, "claude")
	server := agentmcp.Server{Command: "example"}
	for _, agent := range []agentmcp.Agent{agentmcp.AgentCodex, agentmcp.AgentClaude} {
		_, err := agentmcp.Install(agent, agentmcp.InstallOptions{Name: "example", Server: server})
		require.NoError(t, err)
	}
	report, err := Run(t.Context(), Options{Agents: []string{"codex", "claude"}, SetupCommand: "app setup", CheckOnly: true, Items: func(agent string) ([]Item, error) {
		return []Item{MCP(agent, "example", server), MCP(agent, "missing", server)}, nil
	}})
	require.NoError(t, err)
	require.Equal(t, []State{StateOK, StateMissing, StateOK, StateMissing}, []State{report.Rows[0].State, report.Rows[1].State, report.Rows[2].State, report.Rows[3].State})
	require.Equal(t, new(0), report.Rows[0].ToolCount)
	data, err := json.Marshal(report)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(data), `"tool_count"`))
}
