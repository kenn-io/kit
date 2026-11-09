package agentmcp_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/internal/codexapp/testserver"
)

func runCheckHelper(mode string) int {
	if strings.HasPrefix(mode, "claude") {
		if len(os.Args) != 5 || strings.Join(os.Args[1:4], " ") != "mcp get --" || os.Args[4] != "example" {
			return 2
		}
		fmt.Print(os.Getenv("KIT_MCP_CHECK_REPLY"))
		if mode == "claude-failed" {
			return 1
		}
		return 0
	}
	input := bufio.NewScanner(os.Stdin)
	if err := testserver.Handshake(input, os.Stdout); err != nil {
		return 3
	}
	request, err := testserver.Read(input)
	if err != nil || request.Method != "mcpServerStatus/list" || request.Params["serverName"] != "example" || len(request.Params) != 1 {
		return 4
	}
	fmt.Printf("{\"id\":%d,\"result\":%s}\n", request.ID, os.Getenv("KIT_MCP_CHECK_REPLY"))
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

func TestCheckConfigStates(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		agent        agentmcp.Agent
		want         agentmcp.CheckState
	}{
		{"missing file", "", agentmcp.AgentClaude, agentmcp.CheckMissing},
		{"missing name", `{"mcpServers":{"other":{"command":"other"}}}`, agentmcp.AgentClaude, agentmcp.CheckMissing},
		{"outdated Claude", `{"mcpServers":{"example":{"type":"stdio","command":"different"}}}`, agentmcp.AgentClaude, agentmcp.CheckOutdated},
		{"malformed", `{"mcpServers":null}`, agentmcp.AgentClaude, agentmcp.CheckUnknown},
		{"unsupported native check", "", agentmcp.AgentCopilot, agentmcp.CheckUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			t.Setenv("CODEX_HOME", dir)
			t.Setenv("COPILOT_HOME", dir)
			t.Setenv("PATH", t.TempDir()) // Config outcomes must precede starting an agent.
			path, err := agentmcp.ConfigPath(tc.agent)
			require.NoError(t, err)
			if tc.config != "" {
				require.NoError(t, os.WriteFile(path, []byte(tc.config), 0o600))
			}
			result, err := agentmcp.Check(t.Context(), tc.agent, "example", agentmcp.Server{Command: "example"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.State)
			assert.Equal(t, path, result.ConfigPath)
			assert.NotEmpty(t, result.NextStep)
			if tc.config != "" {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, tc.config, string(data))
			} else {
				assert.NoFileExists(t, path)
			}
		})
	}
}

func TestCheckCodex(t *testing.T) {
	testserver.InstallExecutable(t, "codex")
	for _, tc := range []struct {
		name, reply string
		want        agentmcp.CheckState
		count       int
		reason      string
		expired     bool
		config      string
		match       assert.ComparisonAssertionFunc
		next        *string
	}{
		{"user settings and effective defaults", `{"data":[{"name":"example","tools":{}}]}`, agentmcp.CheckOK, 0, "Codex returned the server's tools", false, "[mcp_servers.example]\ncommand='example'\nenabled=true\nargs=[]\nenv={}\nrequired=true\nstartup_timeout_sec=30\nenv_vars=[{name='HOME',source='local'}]\ntools={search={approval_mode='auto'}}\n", assert.Equal, new("")},
		{"changed connection", "", agentmcp.CheckOutdated, 0, "The installed MCP server differs from the requested server", false, "[mcp_servers.example]\ncommand='different'\nrequired=true\n", assert.Equal, new("Reinstall the application's MCP server")},
		{"disabled", "", agentmcp.CheckOutdated, 0, "The installed MCP server differs from the requested server", false, "[mcp_servers.example]\ncommand='example'\nenabled=false\n", assert.Equal, new("Reinstall the application's MCP server")},
		{"catalog readiness", `{"data":[{"name":"example","tools":{"ping":{}}}]}`, agentmcp.CheckUnknown, 0, "Codex catalog readiness does not confirm a live connection", false, "[mcp_servers.example]\ncommand='example'\nstartup_readiness='catalog'\n", assert.Equal, new("Inspect the server in the agent")},
		{"deadline", "", agentmcp.CheckUnknown, 0, "context deadline exceeded", true, "", assert.Contains, nil},
		{"tools", `{"data":[{"name":"example","tools":{"search":{},"read":{}},"toolsError":null}]}`, agentmcp.CheckOK, 2, "Codex returned the server's tools", false, "", assert.Equal, new("")},
		{"missing catalog", `{"data":[{"name":"example"}]}`, agentmcp.CheckUnknown, 0, "Codex did not return the server's tool catalog", false, "", assert.Equal, new("Inspect the server in the agent")},
		{"handshake failure", `{"data":[{"name":"example","tools":{},"toolsError":"MCP startup failed: handshaking with MCP server failed: connection closed: initialize response"}]}`, agentmcp.CheckFailed, 0, "MCP startup failed: handshaking with MCP server failed: connection closed: initialize response", false, "", assert.Equal, new("Correct the server error and retry")},
		{"different server only", `{"data":[{"name":"other","tools":{}}]}`, agentmcp.CheckUnknown, 0, "Codex did not report the named MCP server", false, "", assert.Equal, new("Inspect the server in the agent")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			t.Setenv("KIT_MCP_CHECK_HELPER", "codex")
			t.Setenv("KIT_MCP_CHECK_REPLY", tc.reply)
			installed, err := agentmcp.Install(agentmcp.AgentCodex, agentmcp.InstallOptions{Name: "example", Server: agentmcp.Server{Command: "example"}})
			require.NoError(t, err)
			if tc.config != "" {
				require.NoError(t, os.WriteFile(installed.ConfigPath, []byte(tc.config), 0o600))
			}
			ctx := t.Context()
			if tc.expired {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Time{})
				defer cancel()
			}
			result, err := agentmcp.Check(ctx, agentmcp.AgentCodex, "example", agentmcp.Server{Command: "example"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.State, result.Reason)
			assert.Equal(t, tc.count, result.ToolCount)
			tc.match(t, result.Reason, tc.reason)
			if tc.next != nil {
				assert.Equal(t, *tc.next, result.NextStep)
			}
		})
	}
}

func TestCheckClaude(t *testing.T) {
	testserver.InstallExecutable(t, "claude")
	for _, tc := range []struct {
		name, reply string
		want        agentmcp.CheckState
		reason      string
		mode        string
		expired     bool
		next        string
	}{
		{"deadline", "", agentmcp.CheckUnknown, "context deadline exceeded; server startup may need longer", "claude", true, "Retry once the server is ready"},
		{"connected", "Scope: User config (available in all projects)\nStatus: ✓ Connected\n", agentmcp.CheckOK, "Claude reports the user server connected", "claude", false, ""},
		{"connected check mark", "Scope: User config\nStatus: ✔ Connected\n", agentmcp.CheckOK, "Claude reports the user server connected", "claude", false, ""},
		{"connection failed", "Scope: User config\nStatus: ✗ Failed to connect\n", agentmcp.CheckFailed, "✗ Failed to connect", "claude", false, "Correct the server error and retry"},
		{"tools failed", "Scope: User config\nStatus: ✓ Connected\nIssue: Failed to discover tools: forbidden\n", agentmcp.CheckFailed, "✓ Connected\nFailed to discover tools: forbidden", "claude", false, "Correct the server error and retry"},
		{"project shadows user", "Scope: Project config\nStatus: ✓ Connected\n", agentmcp.CheckUnknown, "Claude reported MCP scope: Project config", "claude", false, "Inspect the server in the agent"},
		{"failed command", "native check refused", agentmcp.CheckUnknown, "exit status 1\nnative check refused", "claude-failed", false, "Inspect the server in the agent"},
		{"hidden user config", "No MCP server named \"example\". Run `claude mcp add` to add one.", agentmcp.CheckUnknown, "exit status 1\nNo MCP server named \"example\". Run `claude mcp add` to add one.", "claude-failed", false, "Check the user config or rerun outside Claude safe mode"},
		{"unrecognized status", "Scope: User config\nStatus: Waiting for approval\n", agentmcp.CheckUnknown, "Claude reported MCP status: Waiting for approval", "claude", false, "Inspect the server in the agent"},
		{"unrecognized output", "future response", agentmcp.CheckUnknown, "Claude reported MCP scope: ", "claude", false, "Inspect the server in the agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv("KIT_MCP_CHECK_HELPER", tc.mode)
			t.Setenv("KIT_MCP_CHECK_REPLY", tc.reply)
			_, err := agentmcp.Install(agentmcp.AgentClaude, agentmcp.InstallOptions{Name: "example", Server: agentmcp.Server{Command: "example"}})
			require.NoError(t, err)
			ctx := t.Context()
			if tc.expired {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Time{})
				defer cancel()
			}
			result, err := agentmcp.Check(ctx, agentmcp.AgentClaude, "example", agentmcp.Server{Command: "example"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.State, result.Reason)
			assert.Equal(t, tc.reason, result.Reason)
			assert.Equal(t, tc.next, result.NextStep)
		})
	}
}
