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
	cmd := consolewindow.CommandContext(ctx, "claude", "mcp", "get", name)
	cmd.WaitDelay = 100 * time.Millisecond
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		result.Reason = ctx.Err().Error()
		return result
	}
	fields := map[string]string{}
	for line := range strings.SplitSeq(string(output), "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), ":")
		fields[key] = strings.TrimSpace(value)
	}
	scope, status, issue := fields["Scope"], fields["Status"], fields["Issue"]
	connected := strings.TrimLeft(status, "✓✔ ") == "Connected"
	switch {
	case scope == "" && err != nil:
		result.Reason = strings.TrimSpace(err.Error() + "\n" + string(output))
	case !strings.HasPrefix(scope, "User config"):
		result.Reason = "Claude reported MCP scope: " + scope
	case issue != "" || status != "" && !connected:
		result.State, result.Reason, result.NextStep = CheckFailed, strings.TrimSpace(status+"\n"+issue), "Correct the server error and retry"
	case connected && err == nil:
		result.State, result.Reason, result.NextStep = CheckOK, "Claude reports the user server connected", ""
	default:
		result.Reason = "Claude reported MCP status: " + status
	}
	return result
}
