package agentmcp

import (
	"context"
	"encoding/json/jsontext"

	"go.kenn.io/kit/internal/codexapp"
)

type codexConfig struct {
	Servers map[string]struct {
		StartupReadiness string `toml:"startup_readiness"`
		Enabled          *bool  `toml:"enabled"`
	} `toml:"mcp_servers"`
}

func checkCodex(ctx context.Context, name string, config codexConfig, result CheckResult) CheckResult {
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
		case server.Tools == nil || config.Servers[name].StartupReadiness == "catalog":
			result.Reason = "Codex couldn't confirm the server's tools are current"
		default:
			result.State, result.Reason, result.NextStep, result.ToolCount = CheckOK, "Codex returned the server's tools", "", len(server.Tools)
		}
		return result
	}
	result.Reason = "Codex didn't report this MCP server"
	return result
}
