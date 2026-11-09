package agentmcp

import (
	"context"
	"errors"
	"os"
)

// CheckState describes the native agent's view of one MCP registration.
type CheckState string

const (
	CheckOK       CheckState = "ok"
	CheckMissing  CheckState = "missing"
	CheckOutdated CheckState = "outdated"
	CheckFailed   CheckState = "failed"
	CheckUnknown  CheckState = "unknown"
)

// CheckResult contains native status and an actionable next step.
// ToolCount is populated for Codex when State is CheckOK.
type CheckResult struct {
	Agent      Agent
	ConfigPath string
	State      CheckState
	Reason     string
	NextStep   string
	ToolCount  int
}

// Check inspects active user config and asks Claude or Codex about one server.
// It writes no config, but the agent may launch the configured server and update
// its own runtime state. A config override outside active user config, an
// unsupported native check, or an inconclusive probe returns CheckUnknown.
func Check(ctx context.Context, agent Agent, opts InstallOptions) (CheckResult, error) {
	entry, err := prepareConfig(agent, opts.Name, opts.Server)
	if err != nil {
		return CheckResult{}, err
	}
	path, err := ConfigPath(agent)
	if err != nil {
		return CheckResult{}, err
	}
	result := CheckResult{Agent: agent, ConfigPath: path, State: CheckUnknown, NextStep: "Inspect the server in the agent"}
	if agent != AgentClaude && agent != AgentCodex {
		result.Reason = "Native MCP inspection is unavailable for this agent"
		return result, nil
	}
	if opts.ConfigPath != "" && opts.ConfigPath != path {
		active, activeErr := os.Stat(path)
		override, overrideErr := os.Stat(opts.ConfigPath)
		same := activeErr == nil && overrideErr == nil && os.SameFile(active, override)
		if !same {
			result.ConfigPath = opts.ConfigPath
			result.Reason = "The requested config is outside the active user config"
			result.NextStep = "Set the agent config home to the requested config directory"
			return result, nil
		}
	}
	original, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	var changed, present bool
	if err == nil {
		_, changed, present, err = planConfig(agent, original, opts.Name, entry)
	}
	switch {
	case err != nil:
		result.Reason, result.NextStep = err.Error(), "Correct the agent MCP config"
	case !present:
		result.State, result.Reason, result.NextStep = CheckMissing, "The named MCP server is missing", "Install the application's MCP server"
	case changed:
		result.State, result.Reason, result.NextStep = CheckOutdated, "The installed MCP server differs from the requested server", "Reinstall the application's MCP server"
	case agent == AgentCodex:
		result = checkCodex(ctx, opts.Name, result)
	default:
		result = checkClaude(ctx, opts.Name, result)
	}
	return result, nil
}

func checkProbeError(err error, result CheckResult) CheckResult {
	result.Reason = err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		result.Reason += "; server startup may need longer"
		result.NextStep = "Retry once the server is ready"
	}
	return result
}
