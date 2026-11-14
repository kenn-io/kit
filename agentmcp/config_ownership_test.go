package agentmcp_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentmcp"
)

func TestInstallEntryOwnership(t *testing.T) {
	for _, tc := range []struct {
		name        string
		agent       agentmcp.Agent
		original    string
		server      agentmcp.Server
		want, exact string
	}{
		{"codex bearer keeps settings", agentmcp.AgentCodex, "[mcp_servers.example]\nurl='https://example.com'\nenabled=false\ntool_timeout_sec=20\n[mcp_servers.example.http_headers]\nUser='keep'\n", agentmcp.Server{URL: "https://example.com", BearerTokenEnvVar: "TOKEN"}, `{"url":"https://example.com","bearer_token_env_var":"TOKEN","enabled":false,"tool_timeout_sec":20,"http_headers":{"User":"keep"}}`, ""},
		{"codex bearer removed", agentmcp.AgentCodex, "[mcp_servers.example]\nurl='https://example.com'\nbearer_token_env_var='TOKEN'\ntool_timeout_sec=20\n", agentmcp.Server{URL: "https://example.com"}, `{"url":"https://example.com","tool_timeout_sec":20}`, ""},
		{"codex bearer replaces authorization", agentmcp.AgentCodex, "[mcp_servers.example]\nurl='https://example.com'\n[mcp_servers.example.http_headers]\nauthorization='Bearer x'\nUser='keep'\n", agentmcp.Server{URL: "https://example.com", BearerTokenEnvVar: "TOKEN"}, `{"url":"https://example.com","bearer_token_env_var":"TOKEN","http_headers":{"User":"keep"}}`, ""},
		{"claude bearer keeps settings", agentmcp.AgentClaude, `{"mcpServers":{"example":{"type":"http","url":"https://example.com","headers":{"X-User":"keep"},"timeout":20}}}`, agentmcp.Server{URL: "https://example.com", BearerTokenEnvVar: "TOKEN"}, `{"type":"http","url":"https://example.com","headers":{"X-User":"keep","Authorization":"Bearer ${TOKEN}"},"timeout":20}`, ""},
		{"claude bearer removed", agentmcp.AgentClaude, `{"mcpServers":{"example":{"type":"http","url":"https://example.com","headers":{"authorization":"Bearer ${T}","X-User":"keep"}}}}`, agentmcp.Server{URL: "https://example.com"}, `{"type":"http","url":"https://example.com","headers":{"X-User":"keep"}}`, ""},
		{"claude bearer-only headers removed", agentmcp.AgentClaude, `{"mcpServers":{"example":{"type":"http","url":"https://example.com","headers":{"Authorization":"Bearer ${USER_TOKEN}"}}}}`, agentmcp.Server{URL: "https://example.com"}, `{"type":"http","url":"https://example.com"}`, ""},
		{"claude literal authorization", agentmcp.AgentClaude, `{"mcpServers":{"example":{"type":"http","url":"https://example.com","headers":{"Authorization":"Bearer abc"}}}}`, agentmcp.Server{URL: "https://example.com"}, `{"type":"http","url":"https://example.com","headers":{"Authorization":"Bearer abc"}}`, ""},
		{"claude launch", agentmcp.AgentClaude, `{"mcpServers":{"example":{"command":"old","env":{"USER":"keep"},"timeout":9007199254740993}}}`, agentmcp.Server{Command: "new"}, `{"type":"stdio","command":"new","env":{"USER":"keep"},"timeout":9007199254740993}`, "9007199254740993"},
		{"claude transport", agentmcp.AgentClaude, `{"mcpServers":{"example":{"command":"old","env":{"USER":"keep"}}}}`, agentmcp.Server{URL: "https://example.com"}, `{"type":"http","url":"https://example.com"}`, ""},
		{"claude http to sse", agentmcp.AgentClaude, `{"mcpServers":{"example":{"type":"http","url":"https://example.com","headers":{"X":"user"}}}}`, agentmcp.Server{URL: "https://example.com", Transport: agentmcp.TransportSSE}, `{"type":"sse","url":"https://example.com"}`, ""},
		{"claude sse to http", agentmcp.AgentClaude, `{"mcpServers":{"example":{"type":"sse","url":"https://example.com","headers":{"X":"user"}}}}`, agentmcp.Server{URL: "https://example.com"}, `{"type":"http","url":"https://example.com"}`, ""},
		{"claude url ampersand", agentmcp.AgentClaude, `{"mcpServers":{"example":{"type":"http","url":"https://example.com/?a=1&b=2","timeout":9007199254740993}}}`, agentmcp.Server{URL: "https://example.com/?a=1&b=3"}, `{"type":"http","url":"https://example.com/?a=1&b=3","timeout":9007199254740993}`, "a=1&b=3"},
		{"other agent", agentmcp.AgentGemini, `{"mcpServers":{"example":{"command":"old","env":{"USER":"keep"}}}}`, agentmcp.Server{Command: "new"}, `{"command":"new"}`, ""},
		{"codex section", agentmcp.AgentCodex, "[mcp_servers.example]\ncommand='old'\nenabled=false\n[mcp_servers.example.env]\nMODE='test'\n", agentmcp.Server{Command: "new"}, `{"command":"new","enabled":false,"env":{"MODE":"test"}}`, ""},
		{"codex dotted", agentmcp.AgentCodex, "mcp_servers.example.command='old'\nmcp_servers.example.env.MODE='test'\n", agentmcp.Server{Command: "new"}, `{"command":"new","env":{"MODE":"test"}}`, ""},
		{"codex inline root", agentmcp.AgentCodex, `mcp_servers = { example = { command = "old", enabled = false, env = {K = "v"}, nested = { x = { y = 2 } }, list = [{ name = "one" }] }, other = { command = "other" } }`, agentmcp.Server{Command: "new"}, `{"command":"new","enabled":false,"env":{"K":"v"},"nested":{"x":{"y":2}},"list":[{"name":"one"}]}`, ""},
		{"codex inline parent", agentmcp.AgentCodex, "[mcp_servers]\nexample = { command = 'old', enabled = false, env = {K = 'v'}, nested = { x = { y = 2 } }, list = [{ name = 'one' }] }\nother = { command = 'other' }\n", agentmcp.Server{Command: "new"}, `{"command":"new","enabled":false,"env":{"K":"v"},"nested":{"x":{"y":2}},"list":[{"name":"one"}]}`, ""},
		{"codex env", agentmcp.AgentCodex, "[mcp_servers.example]\ncommand='old'\nargs=['serve','--write']\nenabled=true\nstartup_timeout_sec=20\n[mcp_servers.example.env]\nUSER='keep'\nAPP='old'\n", agentmcp.Server{Command: "new", Args: []string{"serve"}, Env: map[string]string{"APP": "new", "NEW": "value"}}, `{"command":"new","args":["serve"],"env":{"APP":"new","NEW":"value","USER":"keep"},"enabled":true,"startup_timeout_sec":20}`, ""},
		{"codex headers", agentmcp.AgentCodex, "[mcp_servers.example]\nurl='https://example.com/old'\nenv_http_headers={X='TOKEN'}\ntool_timeout_sec=20\n[mcp_servers.example.http_headers]\nUser='keep'\napp='old'\n", agentmcp.Server{URL: "https://example.com/new", Headers: map[string]string{"App": "new"}}, `{"url":"https://example.com/new","http_headers":{"App":"new","User":"keep"},"env_http_headers":{"X":"TOKEN"},"tool_timeout_sec":20}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			require.NoError(t, os.WriteFile(path, []byte(tc.original), 0o600))
			opts := agentmcp.InstallOptions{ConfigPath: path, Name: "example", Server: tc.server}
			result, err := agentmcp.Install(tc.agent, opts)
			require.NoError(t, err)
			if tc.exact != "" {
				assert.Contains(t, string(result.Data), tc.exact)
				assert.Contains(t, string(result.Data), "9007199254740993")
			}
			key := "mcpServers"
			if tc.agent == agentmcp.AgentCodex {
				key = "mcp_servers"
			}
			servers := decodeConfig(t, tc.agent, result.Data)[key].(map[string]any)
			entry, err := json.Marshal(servers["example"])
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(entry))
			if other, ok := servers["other"]; ok {
				assert.Equal(t, map[string]any{"command": "other"}, other)
			}
			second, err := agentmcp.Install(tc.agent, opts)
			require.NoError(t, err)
			assert.False(t, second.Changed)
			assert.Equal(t, result.Data, second.Data)
		})
	}
}
