package agentmcp_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/internal/codexapp/testserver"
)

func TestCheckConfigStates(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		agent        agentmcp.Agent
		want         agentmcp.CheckState
		reason, next string
	}{
		{"missing file", "", agentmcp.AgentClaude, agentmcp.CheckMissing, "The named MCP server is missing", "Install the application's MCP server"},
		{"missing name", `{"mcpServers":{"other":{"command":"other"}}}`, agentmcp.AgentClaude, agentmcp.CheckMissing, "The named MCP server is missing", "Install the application's MCP server"},
		{"outdated Claude", `{"mcpServers":{"example":{"type":"stdio","command":"different"}}}`, agentmcp.AgentClaude, agentmcp.CheckOutdated, "The MCP server settings don't match this application's setup", "Reinstall the application's MCP server"},
		{"disabled old command", "[mcp_servers.example]\ncommand='different'\nenabled=false\n", agentmcp.AgentCodex, agentmcp.CheckFailed, "The MCP server is disabled", "Set `enabled = true` for `mcp_servers.example` in Codex's config.toml"},
		{"disabled matching command", "[mcp_servers.example]\ncommand='example'\nenabled=false\n", agentmcp.AgentCodex, agentmcp.CheckFailed, "The MCP server is disabled", "Set `enabled = true` for `mcp_servers.example` in Codex's config.toml"},
		{"changed connection", "[mcp_servers.example]\ncommand='different'\n", agentmcp.AgentCodex, agentmcp.CheckOutdated, "The MCP server settings don't match this application's setup", "Reinstall the application's MCP server"},
		{"malformed", `{"mcpServers":null}`, agentmcp.AgentClaude, agentmcp.CheckUnknown, "mcpServers must be an object", "Correct the agent MCP config"},
		{"unsupported native check", "", agentmcp.AgentCopilot, agentmcp.CheckUnknown, "This check can't verify MCP servers in this agent", "Inspect the server in the agent"},
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
			assert.Equal(t, tc.reason, result.Reason)
			assert.Equal(t, tc.next, result.NextStep)
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

func TestCheckMissingAgent(t *testing.T) {
	for _, tc := range []struct {
		agent agentmcp.Agent
		step  string
	}{
		{agentmcp.AgentClaude, "Install Claude Code and make it available on PATH"},
		{agentmcp.AgentCodex, "Install Codex and make it available on PATH"},
	} {
		t.Run(string(tc.agent), func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv("CODEX_HOME", t.TempDir())
			t.Setenv("PATH", t.TempDir())
			server := agentmcp.Server{Command: "example"}
			_, err := agentmcp.Install(tc.agent, agentmcp.InstallOptions{Name: "example", Server: server})
			require.NoError(t, err)
			result, err := agentmcp.Check(t.Context(), tc.agent, "example", server)
			require.NoError(t, err)
			assert.Equal(t, agentmcp.CheckUnknown, result.State)
			assert.Equal(t, tc.step, result.NextStep)
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
		next        string
	}{
		// Matching config must reach the probe despite extra settings and explicit defaults.
		{"matching config with user settings and effective defaults", `{"data":[{"name":"example","tools":{}}]}`, agentmcp.CheckOK, 0, "Codex returned the server's tools", false, "[mcp_servers.example]\ncommand='example'\nenabled=true\nargs=[]\nenv={}\nrequired=true\nstartup_timeout_sec=30\nenv_vars=[{name='HOME',source='local'}]\ntools={search={approval_mode='auto'}}\n", ""},
		{"catalog readiness", `{"data":[{"name":"example","tools":{"ping":{}}}]}`, agentmcp.CheckUnknown, 0, "Codex couldn't confirm the server's tools are current", false, "[mcp_servers.example]\ncommand='example'\nstartup_readiness='catalog'\n", "Inspect the server in the agent"},
		{"deadline", "", agentmcp.CheckUnknown, 0, "context deadline exceeded; server startup may need longer", true, "", "Retry once the server is ready"},
		{"tools", `{"data":[{"name":"example","tools":{"search":{},"read":{}},"toolsError":null}]}`, agentmcp.CheckOK, 2, "Codex returned the server's tools", false, "", ""},
		{"incomplete server status", `{"data":[{"name":"example"}]}`, agentmcp.CheckUnknown, 0, "Codex couldn't confirm the server's tools are current", false, "", "Inspect the server in the agent"},
		{"tools error reported", `{"data":[{"name":"example","tools":{},"toolsError":"MCP startup failed: handshaking with MCP server failed: connection closed: initialize response"}]}`, agentmcp.CheckFailed, 0, "MCP startup failed: handshaking with MCP server failed: connection closed: initialize response", false, "", "Correct the server error and retry"},
		{"server absent from reply", `{"data":[]}`, agentmcp.CheckUnknown, 0, "Codex didn't report this MCP server", false, "", "Inspect the server in the agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			t.Setenv("KIT_MCP_CHECK_HELPER", "codex")
			t.Setenv("KIT_MCP_CHECK_REPLY", tc.reply)
			if tc.config != "" {
				path, err := agentmcp.ConfigPath(agentmcp.AgentCodex)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, []byte(tc.config), 0o600))
			} else {
				_, err := agentmcp.Install(agentmcp.AgentCodex, agentmcp.InstallOptions{Name: "example", Server: agentmcp.Server{Command: "example"}})
				require.NoError(t, err)
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
			assert.Equal(t, tc.reason, result.Reason)
			assert.Equal(t, tc.next, result.NextStep)
		})
	}
}

func TestCheckCodexUserSettings(t *testing.T) {
	testserver.InstallExecutable(t, "codex")
	server := agentmcp.Server{Command: "example", Args: []string{"serve"}, Env: map[string]string{"APP": "current"}, OptionalArgs: []string{"--write", "--archive"}}
	for _, tc := range []struct {
		name, args, app string
		desired         []string
		want            agentmcp.CheckState
	}{
		{"saved flags around command", "'--archive','serve','--write'", "current", nil, agentmcp.CheckOK},
		{"saved equals form", "'--write=true','serve'", "current", nil, agentmcp.CheckOK},
		{"flags first and repeated", "'--write','--write','serve'", "current", nil, agentmcp.CheckOK},
		{"explicit false", "'serve','--write'", "current", []string{"serve", "--write=false"}, agentmcp.CheckOutdated},
		{"user settings", "'serve','--write'", "current", nil, agentmcp.CheckOK},
		{"unlisted arg", "'serve','--other'", "current", nil, agentmcp.CheckOutdated},
		{"missing app env", "'serve','--write'", "", nil, agentmcp.CheckOutdated},
		{"wrong app env", "'serve','--write'", "old", nil, agentmcp.CheckOutdated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", dir)
			t.Setenv("KIT_MCP_CHECK_HELPER", "codex")
			t.Setenv("KIT_MCP_CHECK_REPLY", `{"data":[{"name":"example","tools":{}}]}`)
			config := "[mcp_servers.example]\ncommand='example'\nargs=[" + tc.args + "]\nstartup_timeout_sec=30\n[mcp_servers.example.env]\nUSER='keep'\n"
			if tc.app != "" {
				config += "APP='" + tc.app + "'\n"
			}
			require.NoError(t, os.WriteFile(dir+"/config.toml", []byte(config), 0o600))
			wantServer := server
			if tc.desired != nil {
				wantServer.Args = tc.desired
			}
			result, err := agentmcp.Check(t.Context(), agentmcp.AgentCodex, "example", wantServer)
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.State, result.Reason)
		})
	}
}

func TestCheckClaudeUserSettings(t *testing.T) {
	testserver.InstallExecutable(t, "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("KIT_MCP_CHECK_HELPER", "claude")
	t.Setenv("KIT_MCP_CHECK_REPLY", "Scope: User config\nStatus: ✓ Connected\n")
	path, err := agentmcp.ConfigPath(agentmcp.AgentClaude)
	require.NoError(t, err)
	config := `{"mcpServers":{"example":{"type":"stdio","command":"example","args":["--write=true","serve"],"env":{"APP":"value","USER":"keep"},"timeout":30}}}`
	require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
	server := agentmcp.Server{Command: "example", Args: []string{"serve"}, Env: map[string]string{"APP": "value"}, OptionalArgs: []string{"--write"}}
	result, err := agentmcp.Check(t.Context(), agentmcp.AgentClaude, "example", server)
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

// Run explicitly with KIT_MCP_LIVE_TEST=1 and installed Claude/Codex clients.
func TestInstalledChecks(t *testing.T) {
	for _, client := range []struct {
		agent             agentmcp.Agent
		count             int
		connected, failed string
	}{
		{agentmcp.AgentCodex, 1, "Codex returned the server's tools", "MCP startup failed:"},
		{agentmcp.AgentClaude, 0, "Claude reports the user server connected", "kit-missing-mcp-binary"},
	} {
		t.Run(string(client.agent), func(t *testing.T) {
			setupInstalledAgent(t, client.agent)
			executable, err := os.Executable()
			require.NoError(t, err)
			for _, tc := range []struct {
				name   string
				server agentmcp.Server
				state  agentmcp.CheckState
				count  int
				reason string
			}{
				{"native working", agentmcp.Server{Command: executable, Args: []string{"kit-mcp-stdio"}}, agentmcp.CheckOK, client.count, client.connected},
				{"native missing binary", agentmcp.Server{Command: "kit-missing-mcp-binary"}, agentmcp.CheckFailed, 0, client.failed},
			} {
				t.Run(tc.name, func(t *testing.T) {
					_, err := agentmcp.Install(client.agent, agentmcp.InstallOptions{Name: "native-example", Server: tc.server})
					require.NoError(t, err)
					result, err := agentmcp.Check(t.Context(), client.agent, "native-example", tc.server)
					require.NoError(t, err)
					assert.Equal(t, tc.state, result.State, result.Reason)
					assert.Equal(t, tc.count, result.ToolCount)
					assert.Contains(t, result.Reason, tc.reason)
				})
			}
		})
	}
}
