package agentmcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/BurntSushi/toml"
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
// its own runtime state. An unsupported native check or an inconclusive probe
// returns CheckUnknown.
func Check(ctx context.Context, agent Agent, name string, server Server) (CheckResult, error) {
	entry, err := prepareConfig(agent, name, server)
	if err != nil {
		return CheckResult{}, err
	}
	path, err := ConfigPath(agent)
	if err != nil {
		return CheckResult{}, err
	}
	result := CheckResult{Agent: agent, ConfigPath: path, State: CheckUnknown, NextStep: "Inspect the server in the agent"}
	if agent != AgentClaude && agent != AgentCodex {
		result.Reason = "This check can't verify MCP servers in this agent"
		return result, nil
	}
	original, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	var changed, present bool
	if err == nil && agent == AgentCodex {
		_, changed, present, err = planDocument(agent, original, name, entry, true)
	} else if err == nil {
		_, changed, present, err = planConfig(agent, original, name, entry)
	}
	var config codexConfig
	if err == nil && agent == AgentCodex && present {
		_, err = toml.Decode(string(original), &config)
	}
	switch {
	case err != nil:
		result.Reason, result.NextStep = err.Error(), "Correct the agent MCP config"
	case !present:
		result.State, result.Reason, result.NextStep = CheckMissing, "The named MCP server is missing", "Install the application's MCP server"
	case agent == AgentCodex && config.Servers[name].Enabled != nil && !*config.Servers[name].Enabled:
		result.State, result.Reason, result.NextStep = CheckFailed, "The MCP server is disabled", fmt.Sprintf("Set `enabled = true` for `mcp_servers.%s` in Codex's config.toml", name)
	case changed:
		result.State, result.Reason, result.NextStep = CheckOutdated, "The MCP server settings don't match this application's setup", "Reinstall the application's MCP server"
	case agent == AgentCodex:
		result = checkCodex(ctx, name, config, result)
	default:
		result = checkClaude(ctx, name, result)
	}
	return result, nil
}

func checkProbeError(err error, result CheckResult) CheckResult {
	result.Reason = err.Error()
	if errors.Is(err, exec.ErrNotFound) {
		profile, _ := LookupProfile(result.Agent)
		result.NextStep = "Install " + profile.DisplayName + " and make it available on PATH"
	} else if errors.Is(err, context.DeadlineExceeded) {
		result.Reason += "; server startup may need longer"
		result.NextStep = "Retry once the server is ready"
	}
	return result
}
