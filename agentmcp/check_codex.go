package agentmcp

import (
	"context"
	"encoding/json/jsontext"

	"github.com/BurntSushi/toml"
	"go.kenn.io/kit/internal/codexapp"
)

func checkCodex(ctx context.Context, name string, original []byte, result CheckResult) CheckResult {
	// Check has already decoded this config; catalog readiness can report cached tools.
	var config struct {
		Servers map[string]struct {
			StartupReadiness string `toml:"startup_readiness"`
		} `toml:"mcp_servers"`
	}
	_, _ = toml.Decode(string(original), &config)
	client, err := codexapp.Start(ctx, "codex", "")
	if err != nil {
		return checkProbeError(err, result)
	}
	defer client.Close()
	var listed struct {
		Data []struct {
			Name       string                    `json:"name"`
			Tools      map[string]jsontext.Value `json:"tools"`
			ToolsError *string                   `json:"toolsError"`
		} `json:"data"`
	}
	if err := client.Call("mcpServerStatus/list", map[string]any{"serverName": name}, &listed); err != nil {
		return checkProbeError(err, result)
	}
	for _, server := range listed.Data {
		if server.Name != name {
			continue
		}
		switch {
		case server.ToolsError != nil:
			result.State, result.Reason, result.NextStep = CheckFailed, *server.ToolsError, "Correct the server error and retry"
		case config.Servers[name].StartupReadiness == "catalog":
			result.Reason = "Codex catalog readiness does not confirm a live connection"
		case server.Tools == nil:
			result.Reason = "Codex did not return the server's tool catalog"
		default:
			result.State, result.Reason, result.NextStep, result.ToolCount = CheckOK, "Codex returned the server's tools", "", len(server.Tools)
		}
		return result
	}
	result.Reason = "Codex did not report the named MCP server"
	return result
}
