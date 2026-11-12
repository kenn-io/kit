package agentsetup

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
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

func TestMCPGuidance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	server := agentmcp.Server{Command: "example"}
	_, err := agentmcp.Install(agentmcp.AgentClaude, agentmcp.InstallOptions{Name: "example", Server: server})
	require.NoError(t, err)
	status, err := MCP("claude", "example", server).Check(t.Context())
	require.NoError(t, err)
	require.Equal(t, StateUnknown, status.State)
	require.Contains(t, status.NextStep, "Install Claude Code")
	status, err = MCP("muse", "example", server).Check(t.Context())
	require.NoError(t, err)
	require.Equal(t, StateUnknown, status.State)
	require.Equal(t, "Inspect the server in muse", status.NextStep)
}

func TestAgentNameValidation(t *testing.T) {
	for _, name := range []string{"../future", "future/name", "future;echo", "future$(cmd)", "-option", ""} {
		t.Run(name, func(t *testing.T) {
			called := false
			_, err := Run(t.Context(), Options{Agents: []string{name}, SetupCommand: "app setup", Items: func(string) ([]Item, error) { called = true; return nil, nil }})
			require.Error(t, err)
			require.False(t, called)
		})
	}
	_, err := normalizeAgents([]string{"future-agent_2"})
	require.NoError(t, err)
}

func TestUnknownAgentAndKnownAgent(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	opts := Options{Agents: []string{" Future ", "claude", "FUTURE"}, SetupCommand: "app setup", Items: func(agent string) ([]Item, error) {
		return []Item{Hooks(agent, agenthook.InstallOptions{Executable: "example", Marker: "example", Hooks: []agenthook.Hook{{Event: agenthook.EventStop}}}), MCP(agent, "example", agentmcp.Server{Command: "example"})}, nil
	}}
	report, err := Run(t.Context(), opts)
	require.NoError(t, err)
	require.Len(t, report.Rows, 4)
	require.Equal(t, "future", report.Rows[0].AgentName)
	require.Equal(t, StateUnknown, report.Rows[0].State)
	require.Equal(t, "claude", report.Rows[2].Agent)
	require.Equal(t, StateUnknown, report.Rows[2].State)
	require.Equal(t, StateUnknown, report.Rows[3].State)
}

func TestPrint(t *testing.T) {
	t.Run("cursor then claude", func(t *testing.T) {
		var out bytes.Buffer
		err := Print(&out, Options{Agents: []string{"cursor", "claude"}, Items: func(agent string) ([]Item, error) {
			return []Item{MCP(agent, "example", agentmcp.Server{Command: "example"})}, nil
		}})
		require.NoError(t, err)
		command := "claude mcp add"
		if runtime.GOOS == "windows" {
			command = "& 'claude' 'mcp' 'add'"
		}
		require.Contains(t, out.String(), "# cursor: manual MCP command unavailable for agent \"cursor\"\n"+command)
	})
	for _, tc := range []struct{ name, first, serverName string }{
		{"unknown agent", "future", "example"},
		{"unexpressible name", "claude", "a.b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := Print(&out, Options{Agents: []string{tc.first, "codex"}, Items: func(agent string) ([]Item, error) {
				return []Item{MCP(agent, tc.serverName, agentmcp.Server{Command: "example"})}, nil
			}})
			require.NoError(t, err)
			require.Contains(t, out.String(), "# "+tc.first+": ")
			require.Contains(t, out.String(), "codex")
		})
	}

	t.Run("codex headers", func(t *testing.T) {
		var out bytes.Buffer
		err := Print(&out, Options{Agents: []string{"codex"}, Items: func(agent string) ([]Item, error) {
			return []Item{MCP(agent, "example", agentmcp.Server{URL: "https://example.com", Headers: map[string]string{"Authorization": "example"}})}, nil
		}})
		require.NoError(t, err)
		require.Contains(t, out.String(), "# codex: codex mcp add cannot express literal HTTP headers")
	})
	t.Run("invalid", func(t *testing.T) {
		var out bytes.Buffer
		err := Print(&out, Options{Agents: []string{"claude"}, Items: func(agent string) ([]Item, error) {
			return []Item{MCP(agent, "example", agentmcp.Server{})}, nil
		}})
		require.Error(t, err)
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
