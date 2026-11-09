package agentmcp_test

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
		{agentmcp.AgentClaude, agentmcp.Server{Command: "/a path/example", Args: []string{"a'b", "$HOME"}, Env: map[string]string{"B": "two", "A": "one"}}, `claude mcp add --scope user --env 'A=one' --env 'B=two' example -- '/a path/example' 'a'\''b' '$HOME'`},
		{agentmcp.AgentCodex, agentmcp.Server{Command: "example", Args: []string{"--serve"}}, "codex mcp add example -- example --serve"},
		{agentmcp.AgentCodex, agentmcp.Server{URL: "https://example.com/mcp", BearerTokenEnvVar: "TOKEN"}, "codex mcp add --url https://example.com/mcp --bearer-token-env-var TOKEN -- example"},
		{agentmcp.AgentClaude, agentmcp.Server{URL: "https://example.com/mcp", BearerTokenEnvVar: "TOKEN"}, `claude mcp add --scope user --transport http --header 'Authorization: Bearer ${TOKEN}' -- example https://example.com/mcp`},
	} {
		command, err := agentmcp.Command(tc.agent, "example", tc.server)
		require.NoError(t, err)
		assert.Equal(t, tc.want, command.POSIX)
		assert.Contains(t, command.PowerShell, "& '")
	}
	_, err := agentmcp.Command(agentmcp.AgentCodex, "example", agentmcp.Server{URL: "https://example.com/mcp", Headers: map[string]string{"X-Key": "secret"}})
	require.ErrorContains(t, err, "HTTP headers")
	_, err = agentmcp.Command(agentmcp.AgentCursor, "example", agentmcp.Server{Command: "example"})
	require.Error(t, err)
}

func runStdioFixture() int {
	server := mcp.NewServer(&mcp.Implementation{Name: "kit-test", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "ping", Description: "Test tool"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return &mcp.CallToolResult{}, struct{}{}, nil
	})
	// This standalone fixture has no caller context; EOF closes the session.
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		return 1
	}
	return 0
}

// Run explicitly with KIT_MCP_LIVE_TEST=1 and installed Claude/Codex clients.
func TestInstalledClients(t *testing.T) {
	if os.Getenv("KIT_MCP_LIVE_TEST") != "1" {
		t.Skip("requires installed agents")
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, agent := range []agentmcp.Agent{agentmcp.AgentCodex, agentmcp.AgentClaude} {
		t.Run(string(agent), func(t *testing.T) {
			_, err := exec.LookPath(string(agent))
			require.NoError(t, err)
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", dir)
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
			t.Chdir(dir)
			for _, tc := range []struct {
				name   string
				server agentmcp.Server
				state  agentmcp.CheckState
			}{
				{"working", agentmcp.Server{Command: executable, Args: []string{"kit-mcp-stdio"}}, agentmcp.CheckOK},
				{"missing-binary", agentmcp.Server{Command: "kit-missing-mcp-binary"}, agentmcp.CheckFailed},
			} {
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
					result, err := agentmcp.Check(ctx, agent, opts)
					require.NoError(t, err)
					assert.Equal(t, tc.state, result.State, result.Reason)
					if tc.state == agentmcp.CheckFailed {
						if agent == agentmcp.AgentCodex {
							assert.Contains(t, result.Reason, "MCP startup failed:")
						} else {
							assert.Contains(t, result.Reason, "kit-missing-mcp-binary")
						}
					}
					if agent == agentmcp.AgentCodex && tc.state == agentmcp.CheckOK {
						assert.Equal(t, 1, result.ToolCount)
					}
				})
			}
		})
	}
}
