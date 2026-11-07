package agenthook

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"go.kenn.io/kit/pathresolve"
)

// CheckState describes what prevents an agent from using an application's hooks.
type CheckState string

const (
	CheckOK        CheckState = "ok"
	CheckMissing   CheckState = "missing"
	CheckOutdated  CheckState = "outdated"
	CheckUntrusted CheckState = "untrusted"
	CheckModified  CheckState = "modified"
	CheckDisabled  CheckState = "disabled"
	CheckUnknown   CheckState = "unknown"
)

// CheckResult reports observed hooks, a reason, and a corrective step.
type CheckResult struct {
	Agent      Agent
	ConfigPath string
	State      CheckState
	HookCount  int
	Reason     string
	NextStep   string
}

// Check inspects registrations without running hooks or changing hook config or trust.
// Starting Codex app-server may create state files and install bundled skills.
// Invalid install options return an error. Inspection failures return unknown.
func Check(ctx context.Context, agent Agent, opts InstallOptions) (CheckResult, error) {
	spec, path, hooks, opts, argv, err := prepareRegistration(agent, opts)
	if err != nil {
		return CheckResult{}, err
	}
	result := CheckResult{Agent: agent, ConfigPath: path}
	set := func(state CheckState, reason, step string) (CheckResult, error) {
		result.State, result.Reason, result.NextStep = state, reason, step
		return result, nil
	}
	if agent != AgentClaude && agent != AgentCodex {
		return set(CheckUnknown, "Hook inspection is unavailable for this agent", "Inspect hooks in the agent")
	}
	active, err := ConfigPath(agent)
	if err != nil {
		return set(CheckUnknown, err.Error(), "Check the agent config home")
	}
	if !sameConfigPath(path, active) {
		return set(CheckUnknown, "The requested config is outside the active user config", "Set the agent config home to the requested config directory")
	}
	root, _, err := readJSONConfig(path)
	if err != nil {
		return set(CheckUnknown, err.Error(), "Correct the agent hook config")
	}
	records, err := ownedRegistrations(root, path, opts.Marker)
	if err != nil {
		return set(CheckUnknown, err.Error(), "Correct the agent hook config")
	}
	result.HookCount = len(records)
	if len(records) == 0 {
		return set(CheckMissing, "No hooks belonging to this marker are installed", "Install the application's hooks")
	}
	if agent == AgentClaude {
		disabled, err := claudeHooksDisabled(ctx, root)
		if err != nil {
			return set(CheckUnknown, err.Error(), "Check Claude Code user, project, and local settings")
		}
		if disabled {
			return set(CheckDisabled, "Claude Code settings disable all hooks", "Turn off disableAllHooks in the effective Claude Code settings")
		}
	}
	state, reason, step := CheckOK, "", ""
	if agent == AgentCodex {
		state, reason, step = checkCodex(ctx, path, records)
		if state != CheckOK && state != CheckUntrusted {
			return set(state, reason, step)
		}
	}
	_, changed, err := planConfig(spec, path, opts.Marker, opts.Command, opts.CommandWindows, argv, hooks, false)
	if err != nil {
		return set(CheckUnknown, err.Error(), "Correct the agent hook config")
	}
	if changed {
		return set(CheckOutdated, "Installed hooks differ from the requested registrations", "Reinstall the application's hooks")
	}
	if state == CheckUntrusted {
		return set(state, reason, step)
	}
	executable := strings.TrimSpace(opts.Executable)
	if executable == "" {
		if executable, err = rawHookExecutable(opts.Command, opts.CommandWindows); err != nil {
			return set(CheckUnknown, err.Error(), "Use a literal executable for the hook")
		}
	}
	if _, err := exec.LookPath(executable); err != nil {
		return set(CheckUnknown, fmt.Sprintf("Hook executable %q cannot be resolved in the checking process environment: %v", executable, err), "Install the executable or add its directory to PATH")
	}
	if agent == AgentClaude {
		return set(CheckOK, "Claude Code user registrations match and user, project, and local settings permit hooks; executables resolve in the checking process environment; managed policy and native loading are unverified", "")
	}
	return set(CheckOK, "Codex reports the hooks enabled and reviewed; executables resolve in the checking process environment", "")
}

func sameConfigPath(a, b string) bool {
	a, errA := pathresolve.EvalSymlinksAllowMissing(a)
	b, errB := pathresolve.EvalSymlinksAllowMissing(b)
	return errA == nil && errB == nil && a == b
}

type ownedRegistration struct {
	event               string
	group, handlerIndex int
}

func ownedRegistrations(root map[string]any, path, marker string) ([]ownedRegistration, error) {
	hooks, err := jsonHooksObject(root, path, false)
	if err != nil {
		return nil, err
	}
	var records []ownedRegistration
	for event := range hooks {
		entries, err := jsonEventEntries(hooks, event, path)
		if err != nil {
			return nil, err
		}
		for i, raw := range entries {
			group, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid hook group for %s", event)
			}
			handlers, ok := group["hooks"].([]any)
			if !ok {
				return nil, fmt.Errorf("invalid hook handlers for %s", event)
			}
			for j, raw := range handlers {
				handler, ok := raw.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid hook handler for %s", event)
				}
				if ownsJSONHandler(handler, marker) {
					records = append(records, ownedRegistration{event, i, j})
				}
			}
		}
	}
	return records, nil
}
