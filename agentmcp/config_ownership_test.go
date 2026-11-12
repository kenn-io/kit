package agentmcp_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/internal/codexapp/testserver"
)

func TestInstallPreservesCodexSettings(t *testing.T) {
	testserver.InstallExecutable(t, "codex")
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("KIT_MCP_CHECK_HELPER", "codex")
	t.Setenv("KIT_MCP_CHECK_REPLY", `{"data":[{"name":"example","tools":{}}]}`)
	path, err := agentmcp.ConfigPath(agentmcp.AgentCodex)
	require.NoError(t, err)

	for _, tc := range []struct {
		name, original, expected string
		server                   agentmcp.Server
	}{
		{
			name:     "stdio environment",
			original: "[mcp_servers.example]\ncommand='old'\nargs=['old']\nenabled=true\nstartup_timeout_sec=20\n[mcp_servers.example.env]\nUSER='keep'\nAPP='old'\n",
			expected: `{"command":"new","args":["serve"],"env":{"APP":"new"}}`,
			server:   agentmcp.Server{Command: "new", Args: []string{"serve"}, Env: map[string]string{"APP": "new"}},
		},
		{
			name:     "http headers",
			original: "[mcp_servers.example]\nurl='https://example.com/old'\nenv_http_headers={X='TOKEN'}\ntool_timeout_sec=20\n[mcp_servers.example.http_headers]\nUser='keep'\napp='old'\n",
			expected: `{"url":"https://example.com/new","http_headers":{"App":"new"}}`,
			server:   agentmcp.Server{URL: "https://example.com/new", Headers: map[string]string{"App": "new"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(path, []byte(tc.original), 0o600))
			_, err := agentmcp.Install(agentmcp.AgentCodex, agentmcp.InstallOptions{Name: "example", Server: tc.server})
			require.NoError(t, err)

			data, err := os.ReadFile(path)
			require.NoError(t, err)
			entry := decodeConfig(t, agentmcp.AgentCodex, data)["mcp_servers"].(map[string]any)["example"]
			encoded, err := json.Marshal(entry)
			require.NoError(t, err)
			assert.JSONEq(t, tc.expected, string(encoded))

			checked, err := agentmcp.Check(t.Context(), agentmcp.AgentCodex, "example", tc.server)
			require.NoError(t, err)
			assert.Equal(t, agentmcp.CheckOK, checked.State, checked.Reason)
		})
	}
}

func TestInlineCodexSettingsSurvive(t *testing.T) {
	for _, original := range []string{
		`mcp_servers = { example = { command = "old", enabled = false, env = {K = "v"}, nested = { x = { y = 2 } }, list = [{ name = "one" }] }, other = { command = "other" } }`,
		"[mcp_servers]\nexample = { command = 'old', enabled = false, env = {K = 'v'}, nested = { x = { y = 2 } }, list = [{ name = 'one' }] }\nother = { command = 'other' }\n",
	} {
		t.Run(original[:11], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
			opts := agentmcp.InstallOptions{ConfigPath: path, Name: "example", Server: agentmcp.Server{Command: "new"}}
			result, err := agentmcp.Install(agentmcp.AgentCodex, opts)
			require.NoError(t, err)
			servers := decodeConfig(t, agentmcp.AgentCodex, result.Data)["mcp_servers"].(map[string]any)
			entry, err := json.Marshal(servers["example"])
			require.NoError(t, err)
			assert.JSONEq(t, `{"command":"new","enabled":false,"env":{"K":"v"},"nested":{"x":{"y":2}},"list":[{"name":"one"}]}`, string(entry))
			assert.Equal(t, map[string]any{"command": "other"}, servers["other"])
			second, err := agentmcp.Install(agentmcp.AgentCodex, opts)
			require.NoError(t, err)
			assert.Equal(t, result.Data, second.Data)
		})
	}
}

func TestInstallEntryOwnership(t *testing.T) {
	for _, tc := range []struct {
		name     string
		agent    agentmcp.Agent
		original string
		server   agentmcp.Server
		want     string
	}{
		{"claude launch", agentmcp.AgentClaude, `{"mcpServers":{"example":{"command":"old","env":{"USER":"keep"},"timeout":9007199254740993}}}`, agentmcp.Server{Command: "new"}, `{"type":"stdio","command":"new","env":{"USER":"keep"},"timeout":9007199254740993}`},
		{"claude env", agentmcp.AgentClaude, `{"mcpServers":{"example":{"command":"old","env":{"USER":"keep"}}}}`, agentmcp.Server{Command: "new", Env: map[string]string{"APP": "new"}}, `{"type":"stdio","command":"new","env":{"APP":"new"}}`},
		{"claude transport", agentmcp.AgentClaude, `{"mcpServers":{"example":{"command":"old","env":{"USER":"keep"}}}}`, agentmcp.Server{URL: "https://example.com"}, `{"type":"http","url":"https://example.com"}`},
		{"other agent", agentmcp.AgentGemini, `{"mcpServers":{"example":{"command":"old","env":{"USER":"keep"}}}}`, agentmcp.Server{Command: "new"}, `{"command":"new"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.original), 0o600))
			result, err := agentmcp.Install(tc.agent, agentmcp.InstallOptions{ConfigPath: path, Name: "example", Server: tc.server})
			require.NoError(t, err)
			if tc.name == "claude launch" {
				assert.Contains(t, string(result.Data), "9007199254740993")
			}
			entry := decodeConfig(t, tc.agent, result.Data)["mcpServers"].(map[string]any)["example"]
			encoded, err := json.Marshal(entry)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(encoded))
		})
	}
}
