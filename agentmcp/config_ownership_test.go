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
			expected: `{"command":"new","args":["serve"],"enabled":true,"startup_timeout_sec":20,"env":{"USER":"keep","APP":"new"}}`,
			server:   agentmcp.Server{Command: "new", Args: []string{"serve"}, Env: map[string]string{"APP": "new"}},
		},
		{
			name:     "http headers",
			original: "[mcp_servers.example]\nurl='https://example.com/old'\nenv_http_headers={X='TOKEN'}\ntool_timeout_sec=20\n[mcp_servers.example.http_headers]\nUser='keep'\napp='old'\n",
			expected: `{"url":"https://example.com/new","env_http_headers":{"X":"TOKEN"},"tool_timeout_sec":20,"http_headers":{"User":"keep","App":"new"}}`,
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

func TestInstallHeaderNamesMatchWithoutCase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[mcp_servers.example]\nurl='https://example.com/old'\n[mcp_servers.example.http_headers]\nauthorization='old'\nOther='keep'\n"), 0o600))
	result, err := agentmcp.Install(agentmcp.AgentCodex, agentmcp.InstallOptions{
		ConfigPath: path,
		Name:       "example",
		Server: agentmcp.Server{
			URL:     "https://example.com/new",
			Headers: map[string]string{"Authorization": "new"},
		},
	})
	require.NoError(t, err)
	entry := decodeConfig(t, agentmcp.AgentCodex, result.Data)["mcp_servers"].(map[string]any)["example"]
	encoded, err := json.Marshal(entry)
	require.NoError(t, err)
	assert.JSONEq(t, `{"url":"https://example.com/new","http_headers":{"Authorization":"new","Other":"keep"}}`, string(encoded))
}
