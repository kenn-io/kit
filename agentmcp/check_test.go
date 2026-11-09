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
	}{
		{"deadline", "", agentmcp.CheckUnknown, 0, "startup may need longer", true},
		{"tools", `{"data":[{"name":"example","tools":{"search":{},"read":{}},"toolsError":null}]}`, agentmcp.CheckOK, 2, "", false},
		{"missing catalog", `{"data":[{"name":"example"}]}`, agentmcp.CheckUnknown, 0, "did not return", false},
		{"empty catalog", `{"data":[{"name":"example","tools":{}}]}`, agentmcp.CheckOK, 0, "", false},
		{"handshake failure", `{"data":[{"name":"example","tools":{},"toolsError":"MCP startup failed: handshaking with MCP server failed: connection closed: initialize response"}]}`, agentmcp.CheckFailed, 0, "MCP startup failed: handshaking with MCP server failed: connection closed: initialize response", false},
		{"different server only", `{"data":[{"name":"other","tools":{}}]}`, agentmcp.CheckUnknown, 0, "did not report", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			t.Setenv("KIT_MCP_CHECK_HELPER", "codex")
			t.Setenv("KIT_MCP_CHECK_REPLY", tc.reply)
			_, err := agentmcp.Install(agentmcp.AgentCodex, agentmcp.InstallOptions{Name: "example", Server: agentmcp.Server{Command: "example"}})
			require.NoError(t, err)
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
			assert.Contains(t, result.Reason, tc.reason)
			if tc.expired {
				assert.Equal(t, "Retry once the server is ready", result.NextStep)
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
	}{
		{"deadline", "", agentmcp.CheckUnknown, "startup may need longer", "claude", true},
		{"connected", "Scope: User config (available in all projects)\nStatus: ✓ Connected\n", agentmcp.CheckOK, "", "claude", false},
		{"connected check mark", "Scope: User config\nStatus: ✔ Connected\n", agentmcp.CheckOK, "", "claude", false},
		{"connection failed", "Scope: User config\nStatus: ✗ Failed to connect\n", agentmcp.CheckFailed, "Failed to connect", "claude", false},
		{"tools failed", "Scope: User config\nStatus: ✓ Connected\nIssue: Failed to discover tools: forbidden\n", agentmcp.CheckFailed, "forbidden", "claude", false},
		{"project shadows user", "Scope: Project config\nStatus: ✓ Connected\n", agentmcp.CheckUnknown, "Project", "claude", false},
		{"failed command", "native check refused", agentmcp.CheckUnknown, "native check refused", "claude-failed", false},
		{"unrecognized output", "future response", agentmcp.CheckUnknown, "scope", "claude", false},
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
			assert.Contains(t, result.Reason, tc.reason)
			if tc.expired {
				assert.Equal(t, "Retry once the server is ready", result.NextStep)
			}
		})
	}
}
