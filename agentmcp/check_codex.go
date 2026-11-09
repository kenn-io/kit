package agentmcp

import (
	"cmp"
	"context"
	"encoding/json/jsontext"

	"github.com/BurntSushi/toml"
	"go.kenn.io/kit/internal/codexapp"
)

func checkCodex(ctx context.Context, name, inconclusive string, result CheckResult) CheckResult {
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
		if server.ToolsError != nil {
			result.State, result.Reason, result.NextStep = CheckFailed, *server.ToolsError, "Correct the server error and retry"
		} else if server.Tools == nil || inconclusive != "" {
			result.Reason = cmp.Or(inconclusive, "Codex did not return the server's tool catalog")
		} else {
			result.State, result.Reason, result.NextStep, result.ToolCount = CheckOK, "Codex returned the server's tools", "", len(server.Tools)
		}
		return result
	}
	result.Reason = "Codex did not report the named MCP server"
	return result
}

// codexReadinessReason explains why a returned tool catalog cannot confirm a
// live connection, or returns "" when it can.
func codexReadinessReason(original []byte, name string) string {
	var doc struct {
		Servers map[string]struct {
			StartupReadiness string `toml:"startup_readiness"`
		} `toml:"mcp_servers"`
	}
	if _, err := toml.Decode(string(original), &doc); err != nil || doc.Servers[name].StartupReadiness != "catalog" {
		return ""
	}
	return "Codex catalog readiness does not confirm a live connection"
}
