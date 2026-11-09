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
	if strings.HasPrefix(mode, "claude") {
		name := os.Getenv("KIT_MCP_CHECK_NAME")
		if len(os.Args) != 5 || strings.Join(os.Args[1:4], " ") != "mcp get --" || os.Args[4] != name {
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
		outside      bool
	}{
		{"missing file", "", agentmcp.AgentClaude, agentmcp.CheckMissing, false},
		{"missing name", `{"mcpServers":{"other":{"command":"other"}}}`, agentmcp.AgentClaude, agentmcp.CheckMissing, false},
		{"outdated Claude", `{"mcpServers":{"example":{"type":"stdio","command":"different"}}}`, agentmcp.AgentClaude, agentmcp.CheckOutdated, false},
		{"outside active config", "[mcp_servers.example]\ncommand='example'\n", agentmcp.AgentCodex, agentmcp.CheckUnknown, true},
		{"malformed", `{"mcpServers":null}`, agentmcp.AgentClaude, agentmcp.CheckUnknown, false},
		{"unsupported native check", "", agentmcp.AgentCopilot, agentmcp.CheckUnknown, false},
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
			opts := agentmcp.InstallOptions{Name: "example", Server: agentmcp.Server{Command: "example"}}
			if tc.outside {
				opts.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
				require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(tc.config), 0o600))
				path = opts.ConfigPath
			}
			result, err := agentmcp.Check(t.Context(), tc.agent, opts)
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
	}{
		{"tools", `{"data":[{"name":"example","tools":{"search":{},"read":{}},"toolsError":null}]}`, agentmcp.CheckOK, 2, ""},
		{"missing catalog", `{"data":[{"name":"example"}]}`, agentmcp.CheckUnknown, 0, "did not return"},
		{"null catalog", `{"data":[{"name":"example","tools":null}]}`, agentmcp.CheckUnknown, 0, "did not return"},
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

func TestCheckActiveConfig(t *testing.T) {
	testserver.InstallExecutable(t, "codex")
	t.Setenv("KIT_MCP_CHECK_HELPER", "codex")
	t.Setenv("KIT_MCP_CHECK_REPLY", `{"data":[{"name":"example","tools":{}}]}`)
	t.Setenv("CODEX_HOME", t.TempDir())
	opts := agentmcp.InstallOptions{Name: "example", Server: agentmcp.Server{Command: "example"}}
	installed, err := agentmcp.Install(agentmcp.AgentCodex, opts)
	require.NoError(t, err)
	opts.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
	if err := os.Symlink(installed.ConfigPath, opts.ConfigPath); runtime.GOOS == "windows" && err != nil {
		t.Skip("symlink creation requires privileges on Windows")
	} else {
		require.NoError(t, err)
	}
	result, err := agentmcp.Check(t.Context(), agentmcp.AgentCodex, opts)
	require.NoError(t, err)
	assert.Equal(t, agentmcp.CheckOK, result.State, result.Reason)
}

func TestCheckClaude(t *testing.T) {
	testserver.InstallExecutable(t, "claude")
	for _, tc := range []struct {
		name, reply string
		want        agentmcp.CheckState
		reason      string
		mode        string
	}{
		{"connected", "Scope: User config (available in all projects)\nStatus: ✓ Connected\n", agentmcp.CheckOK, "", "claude"},
		{"connected check mark", "Scope: User config\nStatus: ✔ Connected\n", agentmcp.CheckOK, "", "claude"},
		{"connection failed", "Scope: User config\nStatus: ✗ Failed to connect\n", agentmcp.CheckFailed, "Failed to connect", "claude"},
		{"tools failed", "Scope: User config\nStatus: ✓ Connected\nIssue: Failed to discover tools: forbidden\n", agentmcp.CheckFailed, "forbidden", "claude"},
		{"project shadows user", "Scope: Project config\nStatus: ✓ Connected\n", agentmcp.CheckUnknown, "Project", "claude"},
		{"failed command", "native check refused", agentmcp.CheckUnknown, "native check refused", "claude-failed"},
		{"unrecognized output", "future response", agentmcp.CheckUnknown, "scope", "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv("KIT_MCP_CHECK_HELPER", tc.mode)
			t.Setenv("KIT_MCP_CHECK_REPLY", tc.reply)
			opts := agentmcp.InstallOptions{Name: "example", Server: agentmcp.Server{Command: "example"}}
			t.Setenv("KIT_MCP_CHECK_NAME", opts.Name)
			_, err := agentmcp.Install(agentmcp.AgentClaude, opts)
			require.NoError(t, err)
			result, err := agentmcp.Check(t.Context(), agentmcp.AgentClaude, opts)
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.State, result.Reason)
			assert.Contains(t, result.Reason, tc.reason)
		})
	}
}
