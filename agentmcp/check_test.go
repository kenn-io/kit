package agentmcp_test

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/internal/codexapp/testserver"
)

func runCheckHelper(mode string) int {
	if mode == "claude" {
		if len(os.Args) != 4 || strings.Join(os.Args[1:], " ") != "mcp get example" {
			return 2
		}
		fmt.Print(os.Getenv("KIT_MCP_CHECK_REPLY"))
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

func fakeAgent(t *testing.T, agent string) {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	program, err := os.ReadFile(executable)
	require.NoError(t, err)
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		agent += ".exe"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, agent), program, 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
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
		{"outdated Codex", "[mcp_servers.example]\ncommand='different'\n", agentmcp.AgentCodex, agentmcp.CheckOutdated},
		{"disabled Codex", "[mcp_servers.example]\ncommand='example'\nenabled=false\n", agentmcp.AgentCodex, agentmcp.CheckOutdated},
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
			result, err := agentmcp.Check(t.Context(), tc.agent, agentmcp.InstallOptions{Name: "example", Server: agentmcp.Server{Command: "example"}})
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
	result, err := agentmcp.Check(t.Context(), agentmcp.AgentCodex, agentmcp.InstallOptions{ConfigPath: filepath.Join(t.TempDir(), "config.toml"), Name: "example", Server: agentmcp.Server{Command: "example"}})
	require.NoError(t, err)
	assert.Equal(t, agentmcp.CheckUnknown, result.State)
	assert.Contains(t, result.Reason, "outside the active")
	_, err = agentmcp.Check(t.Context(), agentmcp.AgentClaude, agentmcp.InstallOptions{Name: "example"})
	require.Error(t, err)
}

func TestCheckCodex(t *testing.T) {
	fakeAgent(t, "codex")
	for _, tc := range []struct {
		name, reply string
		want        agentmcp.CheckState
		count       int
		reason      string
	}{
		{"tools", `{"data":[{"name":"example","tools":{"search":{},"read":{}},"toolsError":null}]}`, agentmcp.CheckOK, 2, ""},
		{"empty catalog", `{"data":[{"name":"example","tools":{}}]}`, agentmcp.CheckOK, 0, ""},
		{"handshake failure", `{"data":[{"name":"example","tools":{},"toolsError":"MCP startup failed: handshaking with MCP server failed: connection closed: initialize response"}]}`, agentmcp.CheckFailed, 0, "MCP startup failed: handshaking with MCP server failed: connection closed: initialize response"},
		{"different server only", `{"data":[{"name":"other","tools":{}}]}`, agentmcp.CheckUnknown, 0, "did not report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			t.Setenv("KIT_MCP_CHECK_HELPER", "codex")
			t.Setenv("KIT_MCP_CHECK_REPLY", tc.reply)
			opts := agentmcp.InstallOptions{Name: "example", Server: agentmcp.Server{Command: "example"}}
			_, err := agentmcp.Install(agentmcp.AgentCodex, opts)
			require.NoError(t, err)
			result, err := agentmcp.Check(t.Context(), agentmcp.AgentCodex, opts)
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.State, result.Reason)
			assert.Equal(t, tc.count, result.ToolCount)
			assert.Contains(t, result.Reason, tc.reason)
		})
	}
}

func TestCheckClaude(t *testing.T) {
	fakeAgent(t, "claude")
	for _, tc := range []struct {
		name, reply string
		want        agentmcp.CheckState
		reason      string
	}{
		{"connected", "Scope: User config (available in all projects)\nStatus: ✓ Connected\n", agentmcp.CheckOK, ""},
		{"connected check mark", "Scope: User config\nStatus: ✔ Connected\n", agentmcp.CheckOK, ""},
		{"connection failed", "Scope: User config\nStatus: ✗ Failed to connect\nIssue: spawn example ENOENT\n", agentmcp.CheckFailed, "spawn example ENOENT"},
		{"tools failed", "Scope: User config\nStatus: ✓ Connected\nIssue: Failed to discover tools: forbidden\n", agentmcp.CheckFailed, "forbidden"},
		{"project shadows user", "Scope: Project config\nStatus: ✓ Connected\n", agentmcp.CheckUnknown, "Project"},
		{"unrecognized output", "future response", agentmcp.CheckUnknown, "scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv("KIT_MCP_CHECK_HELPER", "claude")
			t.Setenv("KIT_MCP_CHECK_REPLY", tc.reply)
			opts := agentmcp.InstallOptions{Name: "example", Server: agentmcp.Server{Command: "example"}}
			_, err := agentmcp.Install(agentmcp.AgentClaude, opts)
			require.NoError(t, err)
			result, err := agentmcp.Check(t.Context(), agentmcp.AgentClaude, opts)
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.State, result.Reason)
			assert.Contains(t, result.Reason, tc.reason)
		})
	}
}
