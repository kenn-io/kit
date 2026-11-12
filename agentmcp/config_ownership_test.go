package agentmcp_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/internal/codexapp/testserver"
)

func TestInstallPreservesServerSettings(t *testing.T) {
	for _, tc := range []struct {
		name, original, expected, key string
		agent                         agentmcp.Agent
		server                        agentmcp.Server
	}{
		{"codex stdio", "[mcp_servers.example]\ncommand='old'\nargs=['old']\nenabled=false\nstartup_timeout_sec=20\n[mcp_servers.example.env]\nUSER='keep'\nAPP='old'\n", `{"command":"new","args":["serve"],"enabled":false,"startup_timeout_sec":20,"env":{"USER":"keep","APP":"new"}}`, "mcp_servers", agentmcp.AgentCodex, agentmcp.Server{Command: "new", Args: []string{"serve"}, Env: map[string]string{"APP": "new"}}},
		{"codex http", "[mcp_servers.example]\nurl='https://example.com/old'\nbearer_token_env_var='USER_TOKEN'\nenv_http_headers={X='HEADER_ENV'}\ntool_timeout_sec=20\n[mcp_servers.example.http_headers]\nUser='keep'\nApp='old'\n", `{"url":"https://example.com/new","bearer_token_env_var":"USER_TOKEN","env_http_headers":{"X":"HEADER_ENV"},"tool_timeout_sec":20,"http_headers":{"User":"keep","App":"new"}}`, "mcp_servers", agentmcp.AgentCodex, agentmcp.Server{URL: "https://example.com/new", Headers: map[string]string{"App": "new"}}},
		{"claude transport change", `{"mcpServers":{"example":{"type":"stdio","command":"old","args":["old"],"env":{"USER":"keep"},"headers":{"User":"keep"},"custom":{"value":9007199254740993}}}}`, `{"type":"http","url":"https://example.com/new","env":{"USER":"keep"},"headers":{"User":"keep","App":"new"},"custom":{"value":9007199254740993}}`, "mcpServers", agentmcp.AgentClaude, agentmcp.Server{URL: "https://example.com/new", Headers: map[string]string{"App": "new"}}},
		{"hermes transport change", "mcp_servers:\n  example:\n    url: https://example.com/old\n    transport: sse\n    headers:\n      User: keep\n    env:\n      USER: keep\n", `{"command":"new","env":{"USER":"keep","APP":"new"},"headers":{"User":"keep"}}`, "mcp_servers", agentmcp.AgentHermes, agentmcp.Server{Command: "new", Env: map[string]string{"APP": "new"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			require.NoError(t, os.WriteFile(path, []byte(tc.original), 0o600))
			opts := agentmcp.InstallOptions{ConfigPath: path, Name: "example", Server: tc.server}
			result, err := agentmcp.Install(tc.agent, opts)
			require.NoError(t, err)
			assert.True(t, result.Changed)
			doc := decodeConfig(t, tc.agent, result.Data)
			servers := doc[tc.key].(map[string]any)
			encoded, err := json.Marshal(servers["example"])
			require.NoError(t, err)
			assert.JSONEq(t, tc.expected, string(encoded))
			if tc.name == "claude transport change" {
				assert.Contains(t, string(result.Data), "9007199254740993")
			}
			again, err := agentmcp.Install(tc.agent, opts)
			require.NoError(t, err)
			assert.False(t, again.Changed)
			assert.Equal(t, result.Data, again.Data)
		})
	}
}

func TestCheckAfterInstallPreservesCodexSettings(t *testing.T) {
	testserver.InstallExecutable(t, "codex")
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("KIT_MCP_CHECK_HELPER", "codex")
	t.Setenv("KIT_MCP_CHECK_REPLY", `{"data":[{"name":"example","tools":{}}]}`)
	path, err := agentmcp.ConfigPath(agentmcp.AgentCodex)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("[mcp_servers.example]\ncommand='old'\nenabled=true\n[mcp_servers.example.env]\nUSER='keep'\n"), 0o600))
	server := agentmcp.Server{Command: "new"}
	_, err = agentmcp.Install(agentmcp.AgentCodex, agentmcp.InstallOptions{Name: "example", Server: server})
	require.NoError(t, err)
	result, err := agentmcp.Check(t.Context(), agentmcp.AgentCodex, "example", server)
	require.NoError(t, err)
	assert.Equal(t, agentmcp.CheckOK, result.State)
	require.NoError(t, os.WriteFile(path, []byte("[mcp_servers.example]\ncommand='different'\n[mcp_servers.example.env]\nUSER='keep'\n"), 0o600))
	result, err = agentmcp.Check(t.Context(), agentmcp.AgentCodex, "example", server)
	require.NoError(t, err)
	assert.Equal(t, agentmcp.CheckOutdated, result.State)
}

func TestInstallCodexInlineArrayOfTables(t *testing.T) {
	for _, original := range []string{
		`mcp_servers = { example = { command = "old", env_vars = [{name="A"}, {name="B"}] } }`,
		"[mcp_servers]\nexample = { command = \"old\", env_vars = [{name=\"A\"}, {name=\"B\"}] }\n",
	} {
		t.Run(original[:11], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
			result, err := agentmcp.Install(agentmcp.AgentCodex, agentmcp.InstallOptions{ConfigPath: path, Name: "example", Server: agentmcp.Server{Command: "new"}})
			require.NoError(t, err)
			servers := decodeConfig(t, agentmcp.AgentCodex, result.Data)["mcp_servers"].(map[string]any)
			entry := servers["example"].(map[string]any)
			assert.Equal(t, []any{map[string]any{"name": "A"}, map[string]any{"name": "B"}}, entry["env_vars"])
		})
	}
}

func TestInstalledCodexTransportChanges(t *testing.T) {
	setupInstalledAgent(t, agentmcp.AgentCodex)
	path, err := agentmcp.ConfigPath(agentmcp.AgentCodex)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, original, expected string
		server                   agentmcp.Server
	}{
		{"stdio settings", "[mcp_servers.example]\ncommand='old'\nenabled=false\nenv_vars=['HOME','PATH']\n[mcp_servers.example.env]\nUSER='keep'\nAPP='old'\n", `{"command":"new","enabled":false,"env_vars":["HOME","PATH"],"env":{"USER":"keep","APP":"new"}}`, agentmcp.Server{Command: "new", Env: map[string]string{"APP": "new"}}},
		{"http settings", "[mcp_servers.example]\nurl='https://example.com/old'\nenabled=false\nenv_http_headers={X='TOKEN'}\n[mcp_servers.example.http_headers]\nUser='keep'\nApp='old'\n", `{"url":"https://example.com/mcp","enabled":false,"env_http_headers":{"X":"TOKEN"},"http_headers":{"User":"keep","App":"new"}}`, agentmcp.Server{URL: "https://example.com/mcp", Headers: map[string]string{"App": "new"}}},
		{"stdio to http", "[mcp_servers.example]\ncommand='old'\nargs=['old']\nenv_vars=['HOME']\ncwd='/tmp'\nenabled=false\nstartup_timeout_sec=20\n[mcp_servers.example.env]\nUSER='keep'\n", `{"url":"https://example.com/mcp","enabled":false,"startup_timeout_sec":20,"http_headers":{"App":"new"}}`, agentmcp.Server{URL: "https://example.com/mcp", Headers: map[string]string{"App": "new"}}},
		{"http to stdio", "[mcp_servers.example]\nurl='https://example.com/old'\nbearer_token_env_var='TOKEN'\nenv_http_headers={X='TOKEN'}\nenabled=false\ntool_timeout_sec=20\n[mcp_servers.example.http_headers]\nUser='keep'\n", `{"command":"new","args":["serve"],"enabled":false,"tool_timeout_sec":20,"env":{"APP":"new"}}`, agentmcp.Server{Command: "new", Args: []string{"serve"}, Env: map[string]string{"APP": "new"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(path, []byte(tc.original), 0o600))
			result, err := agentmcp.Install(agentmcp.AgentCodex, agentmcp.InstallOptions{Name: "example", Server: tc.server})
			require.NoError(t, err)
			entry := decodeConfig(t, agentmcp.AgentCodex, result.Data)["mcp_servers"].(map[string]any)["example"]
			encoded, err := json.Marshal(entry)
			require.NoError(t, err)
			assert.JSONEq(t, tc.expected, string(encoded))
			output, err := exec.CommandContext(t.Context(), "codex", "mcp", "list", "--json").CombinedOutput()
			require.NoError(t, err, string(output))
			assert.Contains(t, string(output), `"name": "example"`)
			resultCheck, err := agentmcp.Check(t.Context(), agentmcp.AgentCodex, "example", tc.server)
			require.NoError(t, err)
			assert.Equal(t, agentmcp.CheckOK, resultCheck.State, resultCheck.Reason)
			assert.Equal(t, "Codex returned the server's tools", resultCheck.Reason)
			assert.Zero(t, resultCheck.ToolCount)
		})
	}
}
