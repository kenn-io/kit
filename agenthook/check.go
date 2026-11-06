package agenthook

import (
	"context"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// CheckOptions describes the registrations to inspect in the active user config.
type CheckOptions struct {
	InstallOptions   InstallOptions
	WorkingDirectory string
	CodexExecutable  string
}

// CheckState describes what prevents an agent from using an application's hooks.
type CheckState string

const (
	CheckOK         CheckState = "ok"
	CheckMissing    CheckState = "missing"
	CheckOutdated   CheckState = "outdated"
	CheckUntrusted  CheckState = "untrusted"
	CheckModified   CheckState = "modified"
	CheckDisabled   CheckState = "disabled"
	CheckUnresolved CheckState = "unresolved"
	CheckUnknown    CheckState = "unknown"
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

// Check observes registrations without running hooks or changing config or trust.
// Invalid install options return an error. Inspection failures return unknown.
func Check(ctx context.Context, agent Agent, options CheckOptions) (CheckResult, error) {
	_, path, hooks, opts, argv, err := prepareRegistration(agent, options.InstallOptions)
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
	installed, records, err := ownedRegistrations(root, path, opts.Marker)
	if err != nil {
		return set(CheckUnknown, err.Error(), "Correct the agent hook config")
	}
	result.HookCount = len(records)
	expected := map[string]any{}
	if err := applyNestedJSONHooks(expected, path, opts.Marker, opts.Command, opts.CommandWindows, argv, hooks, false); err != nil {
		return CheckResult{}, err
	}
	desired, _, err := ownedRegistrations(expected, path, opts.Marker)
	if err != nil {
		return CheckResult{}, err
	}
	if agent == AgentCodex {
		state, reason, step := checkCodex(ctx, path, options, records)
		if state != CheckOK {
			return set(state, reason, step)
		}
	} else if disabled, _ := root["disableAllHooks"].(bool); disabled {
		return set(CheckDisabled, "Claude Code disables all hooks", "Turn off disableAllHooks in Claude Code settings")
	}
	if len(records) == 0 {
		return set(CheckMissing, "No hooks belonging to this marker are installed", "Install the application's hooks")
	}
	if !slices.Equal(installed, desired) {
		return set(CheckOutdated, "Installed hooks differ from the requested registrations", "Reinstall the application's hooks")
	}
	for _, record := range records {
		executable := strings.TrimSpace(opts.Executable)
		var err error
		if executable == "" {
			executable, err = hookExecutable(record.handler)
		}
		if err != nil {
			return set(CheckUnknown, err.Error(), "Use an executable with literal arguments for the hook")
		}
		if strings.ContainsAny(executable, `/\\`) && !filepath.IsAbs(executable) {
			return set(CheckUnknown, "The hook executable depends on its runtime working directory", "Use an absolute executable path or a name on PATH")
		}
		if _, err := exec.LookPath(executable); err != nil {
			return set(CheckUnresolved, fmt.Sprintf("Hook executable %q cannot be resolved: %v", executable, err), "Install the executable or add its directory to PATH")
		}
	}
	if agent == AgentClaude {
		return set(CheckOK, "Claude Code user config matches and hook executables resolve; native loading is unverified", "")
	}
	return set(CheckOK, "Codex reports the hooks enabled and reviewed; executables resolve", "")
}

func sameConfigPath(a, b string) bool {
	a, errA := filepath.Abs(a)
	b, errB := filepath.Abs(b)
	return errA == nil && errB == nil && a == b
}

type ownedRegistration struct {
	event               string
	group, handlerIndex int
	handler             map[string]any
}

// Keep owned handlers together within each group. Group fields affect all its
// handlers; unrelated commands and group positions do not affect freshness.
func ownedRegistrations(root map[string]any, path, marker string) ([]string, []ownedRegistration, error) {
	hooks, err := jsonHooksObject(root, path, false)
	if err != nil {
		return nil, nil, err
	}
	var groups []string
	var records []ownedRegistration
	for event := range hooks {
		entries, err := jsonEventEntries(hooks, event, path)
		if err != nil {
			return nil, nil, err
		}
		for i, raw := range entries {
			group, ok := raw.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("invalid hook group for %s", event)
			}
			handlers, ok := group["hooks"].([]any)
			if !ok {
				return nil, nil, fmt.Errorf("invalid hook handlers for %s", event)
			}
			var owned []any
			for j, raw := range handlers {
				handler, ok := raw.(map[string]any)
				if !ok {
					return nil, nil, fmt.Errorf("invalid hook handler for %s", event)
				}
				command, _ := handler["command"].(string)
				switch args := handler["args"].(type) {
				case []string:
					command += " " + strings.Join(args, " ")
				case []any:
					var joined strings.Builder
					joined.WriteString(command)
					for _, arg := range args {
						text, _ := arg.(string)
						joined.WriteString(" " + text)
					}
					command = joined.String()
				}
				if strings.Contains(command, marker) {
					owned = append(owned, handler)
					records = append(records, ownedRegistration{event, i, j, handler})
				}
			}
			if len(owned) == 0 {
				continue
			}
			selected := maps.Clone(group)
			selected["hooks"] = owned
			if selected["matcher"] == "" {
				delete(selected, "matcher")
			}
			data, err := marshalJSONConfig(map[string]any{event: selected})
			if err != nil {
				return nil, nil, err
			}
			groups = append(groups, string(data))
		}
	}
	slices.Sort(groups)
	return groups, records, nil
}
