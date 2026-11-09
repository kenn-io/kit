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
		name          string
		agent         agentmcp.Agent
		serverName    string
		server        agentmcp.Server
		want, wantErr string
	}{
		{"claude stdio", agentmcp.AgentClaude, "example", agentmcp.Server{Command: "example", Args: []string{"--serve"}, Env: map[string]string{"B": "two", "A": "one"}}, `claude mcp add --scope user --env 'A=one' --env 'B=two' -- example example --serve`, ""},
		{"codex stdio", agentmcp.AgentCodex, "example", agentmcp.Server{Command: "example", Args: []string{"--serve"}}, "codex mcp add -- example example --serve", ""},
		{"codex bearer HTTP", agentmcp.AgentCodex, "example", agentmcp.Server{URL: "https://example.com/mcp", BearerTokenEnvVar: "TOKEN"}, "codex mcp add --url https://example.com/mcp --bearer-token-env-var TOKEN -- example", ""},
		{"claude bearer HTTP", agentmcp.AgentClaude, "example", agentmcp.Server{URL: "https://example.com/mcp", BearerTokenEnvVar: "TOKEN"}, `claude mcp add --scope user --transport http --header 'Authorization: Bearer ${TOKEN}' -- example https://example.com/mcp`, ""},
		{"claude rejects dots", agentmcp.AgentClaude, "a.b", agentmcp.Server{Command: "example"}, "", "server name"},
		{"claude rejects non-ASCII", agentmcp.AgentClaude, "café", agentmcp.Server{Command: "example"}, "", "server name"},
		{"codex rejects hash", agentmcp.AgentCodex, "a#b", agentmcp.Server{Command: "example"}, "", "server name"},
		{"codex accepts punctuation", agentmcp.AgentCodex, "a.b@c:d/e", agentmcp.Server{Command: "example"}, "codex mcp add -- 'a.b@c:d/e' example", ""},
		{"codex rejects literal HTTP headers", agentmcp.AgentCodex, "example", agentmcp.Server{URL: "https://example.com/mcp", Headers: map[string]string{"X-Key": "secret"}}, "", "HTTP headers"},
		{"cursor unavailable", agentmcp.AgentCursor, "example", agentmcp.Server{Command: "example"}, "", "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command, err := agentmcp.Command(tc.agent, tc.serverName, tc.server)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, command.POSIX)
			}
		})
	}
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
			t.Setenv("CLAUDE_CODE_SAFE_MODE", "")
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
			executable, err := os.Executable()
			require.NoError(t, err)
			for _, tc := range []struct {
				name   string
				server agentmcp.Server
				state  agentmcp.CheckState
			}{
				{"native working", agentmcp.Server{Command: executable, Args: []string{"kit-mcp-stdio"}}, agentmcp.CheckOK},
				{"native missing binary", agentmcp.Server{Command: "kit-missing-mcp-binary"}, agentmcp.CheckFailed},
			} {
				t.Run(tc.name, func(t *testing.T) {
					_, err := agentmcp.Install(agent, agentmcp.InstallOptions{Name: "native-example", Server: tc.server})
					require.NoError(t, err)
					result, err := agentmcp.Check(t.Context(), agent, "native-example", tc.server)
					require.NoError(t, err)
					assert.Equal(t, tc.state, result.State, result.Reason)
					if tc.state == agentmcp.CheckOK && agent == agentmcp.AgentCodex {
						assert.Equal(t, 1, result.ToolCount)
					} else if tc.state == agentmcp.CheckFailed {
						if agent == agentmcp.AgentCodex {
							assert.Contains(t, result.Reason, "MCP startup failed:")
						} else {
							assert.Contains(t, result.Reason, "kit-missing-mcp-binary")
						}
					}
				})
			}
		})
	}
}
