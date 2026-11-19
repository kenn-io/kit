package agentmcp

import (
	"context"
	"strings"
	"time"

	"go.kenn.io/kit/consolewindow"
)

func checkClaude(ctx context.Context, name string, result CheckResult) CheckResult {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := consolewindow.CommandContext(ctx, "claude", "mcp", "get", "--", name)
	cmd.WaitDelay = 100 * time.Millisecond
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return checkProbeError(ctx.Err(), result)
	}
	if err != nil && len(output) == 0 {
		return checkProbeError(err, result)
	}
	fields := map[string]string{}
	for line := range strings.SplitSeq(string(output), "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), ":")
		fields[key] = strings.TrimSpace(value)
	}
	scope, status, issue := fields["Scope"], fields["Status"], fields["Issue"]
	switch {
	case scope == "" && err != nil && strings.Contains(string(output), "No MCP server named"):
		result.Reason, result.NextStep = strings.TrimSpace(err.Error()+"\n"+string(output)), "Check the user config or rerun outside Claude safe mode"
	case scope == "" && err != nil:
		result.Reason = strings.TrimSpace(err.Error() + "\n" + string(output))
	case !strings.HasPrefix(scope, "User config"):
		result.Reason = "Claude reported MCP scope: " + scope
	case issue != "" || strings.TrimLeft(status, "✗✘ ") == "Failed to connect":
		result.State, result.Reason, result.NextStep = CheckFailed, strings.TrimSpace(status+"\n"+issue), "Correct the server error and retry"
	case strings.TrimLeft(status, "✓✔ ") == "Connected":
		result.State, result.Reason, result.NextStep = CheckOK, "Claude reports the user server connected", ""
	default:
		result.Reason = "Claude reported MCP status: " + status
	}
	return result
}
