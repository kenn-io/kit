package agentmcp_test

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentmcp"
)

func TestCommand(t *testing.T) {
	for _, tc := range []struct {
		agent  agentmcp.Agent
		server agentmcp.Server
		want   string
	}{
		{agentmcp.AgentClaude, agentmcp.Server{Command: "example", Args: []string{"--serve"}, Env: map[string]string{"B": "two", "A": "one"}}, `claude mcp add --scope user --env 'A=one' --env 'B=two' -- example example --serve`},
		{agentmcp.AgentCodex, agentmcp.Server{Command: "example", Args: []string{"--serve"}}, "codex mcp add -- example example --serve"},
		{agentmcp.AgentCodex, agentmcp.Server{URL: "https://example.com/mcp", BearerTokenEnvVar: "TOKEN"}, "codex mcp add --url https://example.com/mcp --bearer-token-env-var TOKEN -- example"},
		{agentmcp.AgentClaude, agentmcp.Server{URL: "https://example.com/mcp", BearerTokenEnvVar: "TOKEN"}, `claude mcp add --scope user --transport http --header 'Authorization: Bearer ${TOKEN}' -- example https://example.com/mcp`},
	} {
		command, err := agentmcp.Command(tc.agent, "example", tc.server)
		require.NoError(t, err)
		assert.Equal(t, tc.want, command.POSIX)
	}
	for _, tc := range []struct {
		agent agentmcp.Agent
		name  string
	}{
		{agentmcp.AgentClaude, "a.b"},
		{agentmcp.AgentClaude, "café"},
		{agentmcp.AgentCodex, "a#b"},
	} {
		_, err := agentmcp.Command(tc.agent, tc.name, agentmcp.Server{Command: "example"})
		require.ErrorContains(t, err, "server name")
	}
	_, err := agentmcp.Command(agentmcp.AgentCodex, "a.b@c:d/e", agentmcp.Server{Command: "example"})
	require.NoError(t, err)
	_, err = agentmcp.Command(agentmcp.AgentCodex, "example", agentmcp.Server{URL: "https://example.com/mcp", Headers: map[string]string{"X-Key": "secret"}})
	require.ErrorContains(t, err, "HTTP headers")
	_, err = agentmcp.Command(agentmcp.AgentCursor, "example", agentmcp.Server{Command: "example"})
	require.ErrorContains(t, err, "unavailable")
}

// Run explicitly with KIT_MCP_LIVE_TEST=1 and installed Claude/Codex clients.
func TestInstalledClients(t *testing.T) {
	if os.Getenv("KIT_MCP_LIVE_TEST") != "1" {
		t.Skip("requires installed agents")
	}
	for _, agent := range []agentmcp.Agent{agentmcp.AgentCodex, agentmcp.AgentClaude} {
		t.Run(string(agent), func(t *testing.T) {
			_, err := exec.LookPath(string(agent))
			require.NoError(t, err)
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", dir)
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
			t.Setenv("KIT_MCP_TOKEN", "fixture-token")
			t.Chdir(dir)
			type testCase struct {
				name   string
				server agentmcp.Server
			}
			cases := []testCase{
				{"working", agentmcp.Server{Command: "example", Args: []string{"--serve"}, Env: map[string]string{"KIT_MCP_FIXTURE": "value with spaces"}}},
				{"-leading", agentmcp.Server{Command: "example", Args: []string{"--serve"}}},
				{"http", agentmcp.Server{URL: "https://example.com/mcp"}},
				{"http-bearer", agentmcp.Server{URL: "https://example.com/mcp", BearerTokenEnvVar: "KIT_MCP_TOKEN"}},
			}
			if agent == agentmcp.AgentClaude {
				cases = append(cases, testCase{"http-header", agentmcp.Server{URL: "https://example.com/mcp", Headers: map[string]string{"X-Test": "fixture header"}}})
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					opts := agentmcp.InstallOptions{Name: tc.name, Server: tc.server}
					command, err := agentmcp.Command(agent, opts.Name, opts.Server)
					require.NoError(t, err)
					ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
					defer cancel()
					shell, args := "sh", []string{"-c", command.POSIX}
					if runtime.GOOS == "windows" {
						shell, args = "powershell.exe", []string{"-NoProfile", "-Command", command.PowerShell}
					}
					output, err := exec.CommandContext(ctx, shell, args...).CombinedOutput()
					require.NoError(t, err, string(output))
					planned, err := agentmcp.PlanInstall(agent, opts)
					require.NoError(t, err)
					require.False(t, planned.Changed, "the manual command must register the same server")
				})
			}
		})
	}
}
