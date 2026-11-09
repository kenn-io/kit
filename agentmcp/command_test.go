package agentmcp_test

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/google/shlex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentmcp"
)

func TestCommand(t *testing.T) {
	for _, tc := range []struct {
		name       string
		agent      agentmcp.Agent
		serverName string
		server     agentmcp.Server
		want       []string
		wantErr    string
	}{
		{"claude stdio", agentmcp.AgentClaude, "example", agentmcp.Server{Command: "example", Args: []string{"--serve"}, Env: map[string]string{"B": "two", "A": "one"}}, []string{"claude", "mcp", "add", "--scope", "user", "--env", "A=one", "--env", "B=two", "--", "example", "example", "--serve"}, ""},
		{"codex stdio", agentmcp.AgentCodex, "example", agentmcp.Server{Command: "example", Args: []string{"--serve"}}, []string{"codex", "mcp", "add", "--", "example", "example", "--serve"}, ""},
		{"codex bearer HTTP", agentmcp.AgentCodex, "example", agentmcp.Server{URL: "https://example.com/mcp", BearerTokenEnvVar: "TOKEN"}, []string{"codex", "mcp", "add", "--url", "https://example.com/mcp", "--bearer-token-env-var", "TOKEN", "--", "example"}, ""},
		{"claude bearer HTTP", agentmcp.AgentClaude, "example", agentmcp.Server{URL: "https://example.com/mcp", BearerTokenEnvVar: "TOKEN"}, []string{"claude", "mcp", "add", "--scope", "user", "--transport", "http", "--header", "Authorization: Bearer ${TOKEN}", "--", "example", "https://example.com/mcp"}, ""},
		{"claude rejects dots", agentmcp.AgentClaude, "a.b", agentmcp.Server{Command: "example"}, nil, "server name"},
		{"claude rejects non-ASCII", agentmcp.AgentClaude, "café", agentmcp.Server{Command: "example"}, nil, "server name"},
		{"codex rejects hash", agentmcp.AgentCodex, "a#b", agentmcp.Server{Command: "example"}, nil, "server name"},
		{"codex accepts punctuation", agentmcp.AgentCodex, "a.b@c:d/e", agentmcp.Server{Command: "example"}, []string{"codex", "mcp", "add", "--", "a.b@c:d/e", "example"}, ""},
		{"codex rejects literal HTTP headers", agentmcp.AgentCodex, "example", agentmcp.Server{URL: "https://example.com/mcp", Headers: map[string]string{"X-Key": "secret"}}, nil, "HTTP headers"},
		{"cursor unavailable", agentmcp.AgentCursor, "example", agentmcp.Server{Command: "example"}, nil, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command, err := agentmcp.Command(tc.agent, tc.serverName, tc.server)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				argv, err := shlex.Split(command.POSIX)
				require.NoError(t, err)
				assert.Equal(t, tc.want, argv)
			}
		})
	}
}

// Run explicitly with KIT_MCP_LIVE_TEST=1 and installed Claude/Codex clients.
func TestInstalledClients(t *testing.T) {
	for _, agent := range []agentmcp.Agent{agentmcp.AgentCodex, agentmcp.AgentClaude} {
		t.Run(string(agent), func(t *testing.T) {
			setupInstalledAgent(t, agent)
			t.Setenv("KIT_MCP_TOKEN", "fixture-token")
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
