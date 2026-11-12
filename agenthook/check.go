package agenthook

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

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
// A transient failure can cause Check to start Codex a second time.
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
	cwd, err := filepath.Abs(opts.WorkspaceDir)
	if err != nil {
		return set(CheckUnknown, fmt.Sprintf("Can't resolve workspace %q: %v", opts.WorkspaceDir, err), "Choose a working directory for the hook check")
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return set(CheckUnknown, fmt.Sprintf("Can't access workspace %q: %v", cwd, err), "Choose a working directory for the hook check")
	}
	if !info.IsDir() {
		return set(CheckUnknown, fmt.Sprintf("Workspace %q is not a directory", cwd), "Choose a working directory for the hook check")
	}
	if len(records) == 0 {
		return set(CheckMissing, "This application's hooks are not installed", "Install the application's hooks")
	}
	if agent == AgentClaude {
		disabled, err := claudeHooksDisabled(ctx, root, cwd)
		if err != nil {
			return set(CheckUnknown, err.Error(), "Check Claude Code user, project, and local settings")
		}
		if disabled {
			return set(CheckDisabled, "Claude Code settings turn off all hooks", "Set disableAllHooks to false in the Claude Code settings that apply here")
		}
	}
	state, reason, step := CheckOK, "", ""
	if agent == AgentCodex {
		state, reason, step = checkCodex(ctx, path, cwd, records)
		if state != CheckOK && state != CheckUntrusted {
			return set(state, reason, step)
		}
	}
	_, changed, err := planConfig(spec, path, opts.Marker, opts.Command, opts.CommandWindows, argv, hooks, false, cwd)
	if err != nil {
		return set(CheckUnknown, err.Error(), "Correct the agent hook config")
	}
	if changed {
		return set(CheckOutdated, "The installed hooks don't match this application's setup", "Reinstall the application's hooks")
	}
	if state == CheckUntrusted {
		return set(state, reason, step)
	}
	for _, record := range records {
		words, err := hookWords(record.handler)
		if err != nil {
			return set(CheckUnknown, err.Error(), "Use a literal executable for the hook")
		}
		if err := lookupHookExecutable(cwd, words[0]); err != nil {
			return set(CheckUnknown, fmt.Sprintf("Can't find a usable hook command %q: %v", words[0], err), "Install the executable or add its directory to PATH")
		}
	}
	if agent == AgentClaude {
		return set(CheckOK, "Claude Code's user, project, and local settings allow these hooks, and this process can find their command. Organization policy and whether Claude Code loads them have not been checked", "")
	}
	return set(CheckOK, "Codex reports these hooks enabled and trusted, and this process can find their command", "")
}

func sameConfigPath(a, b string) bool {
	infoA, statA := os.Stat(a)
	infoB, statB := os.Stat(b)
	if statA == nil && statB == nil {
		return os.SameFile(infoA, infoB)
	}
	a, errA := pathresolve.EvalSymlinksAllowMissing(a)
	b, errB := pathresolve.EvalSymlinksAllowMissing(b)
	return errA == nil && errB == nil && a == b
}

type ownedRegistration struct {
	event               string
	group, handlerIndex int
	handler             map[string]any
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
					records = append(records, ownedRegistration{event, i, j, handler})
				}
			}
		}
	}
	return records, nil
}
