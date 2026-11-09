package agentmcp

import (
	"context"
	"encoding/json/jsontext"

	"go.kenn.io/kit/internal/codexapp"
)

func checkCodex(ctx context.Context, name string, result CheckResult) CheckResult {
	client, err := codexapp.Start(ctx, "codex", "")
	if err != nil {
		result.Reason = err.Error()
		return result
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
		result.Reason = err.Error()
		return result
	}
	for _, server := range listed.Data {
		if server.Name != name {
			continue
		}
		if server.ToolsError != nil {
			result.State, result.Reason, result.NextStep = CheckFailed, *server.ToolsError, "Correct the server error and retry"
		} else {
			result.State, result.Reason, result.NextStep, result.ToolCount = CheckOK, "Codex returned the server's tools", "", len(server.Tools)
		}
		return result
	}
	result.Reason = "Codex did not report the named MCP server"
	return result
}
