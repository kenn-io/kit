package agenthook

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"go.kenn.io/kit/internal/codexapp"
)

func codexProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentCodex, DisplayName: "Codex",
			ConfigEnvironment: "CODEX_HOME", ConfigFilename: "hooks.json",
			SupportedEvents: []Event{
				EventSessionStart, EventUserPromptSubmit, EventPreToolUse,
				EventPostToolUse, EventPermissionRequest, EventStop, EventSessionEnd,
			},
		},
		formatNestedJSON,
		"^Bash$",
		func() (string, error) { return userDotDir(".codex") },
	)
	spec.windowsCommandStyle = windowsCommandNested
	spec.shellToolName = ToolBash
	// Codex hooks use the Claude-compatible JSON response and lifecycle input
	// contract, including SessionStart source and SessionEnd reason:
	// https://developers.openai.com/codex/config-advanced#hooks
	spec.responseFormat = responseClaude
	spec.sessionSourceRequirement = inputRequired
	spec.sessionEndReasonRequirement = inputRequired
	return spec
}

// checkCodex reads native discovery and consent state. It never writes trust.
func checkCodex(ctx context.Context, path, executable string, owned []ownedRegistration) (CheckState, string, string) {
	if executable == "" {
		executable = "codex"
	}
	cwd, err := os.Getwd()
	if err != nil {
		return CheckUnknown, err.Error(), "Choose a working directory for Codex"
	}
	client, err := codexapp.Start(ctx, executable, cwd)
	if err != nil {
		step := "Check Codex app-server startup and retry"
		var pathError *os.PathError
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			step = "Allow more time for Codex to start and retry"
		case errors.Is(err, exec.ErrNotFound), errors.Is(err, os.ErrNotExist):
			step = "Install Codex and make it available on PATH"
		case errors.As(err, &pathError) && pathError.Op == "fork/exec":
			step = "Check the Codex executable path and permissions and retry"
		}
		return CheckUnknown, err.Error(), step
	}
	defer client.Close()
	var config struct {
		Config *struct {
			Features map[string]*bool `json:"features"`
		} `json:"config"`
	}
	if err := client.Call("config/read", map[string]any{"includeLayers": false, "cwd": cwd}, &config); err != nil {
		return CheckUnknown, err.Error(), "Check Codex config and retry"
	}
	if config.Config == nil {
		return CheckUnknown, "Codex did not report its effective hooks feature", "Check Codex config and retry"
	}
	if enabled := config.Config.Features["hooks"]; enabled != nil && !*enabled {
		return CheckDisabled, "Codex hooks feature is disabled for this workspace", "Enable features.hooks in the effective Codex config for this workspace"
	}
	var listed struct {
		Data []struct {
			Hooks []struct {
				Key         string `json:"key"`
				Source      string `json:"source"`
				SourcePath  string `json:"sourcePath"`
				TrustStatus string `json:"trustStatus"`
				Enabled     bool   `json:"enabled"`
			} `json:"hooks"`
			Errors []jsontext.Value `json:"errors"`
		} `json:"data"`
	}
	if err := client.Call("hooks/list", map[string]any{"cwds": []string{cwd}}, &listed); err != nil {
		return CheckUnknown, err.Error(), "Check Codex hook discovery and retry"
	}
	if len(listed.Data) != 1 {
		return CheckUnknown, "Codex did not report hooks for the requested directory", "Check Codex hook discovery and retry"
	}
	entry := listed.Data[0]
	if len(entry.Errors) > 0 {
		return CheckUnknown, fmt.Sprintf("Codex hook discovery errors: %s", entry.Errors), "Correct the reported hook discovery errors"
	}
	states := make(map[string]bool)
	count := 0
	for _, hook := range entry.Hooks {
		if hook.Source != "user" || !sameConfigPath(hook.SourcePath, path) {
			continue
		}
		suffix, ok := strings.CutPrefix(hook.Key, hook.SourcePath+":")
		if !ok {
			continue
		}
		for _, registration := range owned {
			key := fmt.Sprintf("%s:%d:%d", strings.ToLower(registration.event), registration.group, registration.handlerIndex)
			if strings.ReplaceAll(suffix, "_", "") != key {
				continue
			}
			count++
			if !hook.Enabled {
				states["disabled"] = true
			}
			switch hook.TrustStatus {
			case "trusted", "managed", "untrusted", "modified":
				states[hook.TrustStatus] = true
			default:
				return CheckUnknown, "Codex reported unknown hook trust state: " + hook.TrustStatus, "Review hooks in Codex with /hooks"
			}
		}
	}
	if count == 0 {
		return CheckMissing, "Codex did not discover this application's hooks", "Install the application's hooks"
	}
	if count != len(owned) {
		return CheckUnknown, "Codex discovered only some of the installed hooks", "Review Codex hook discovery with /hooks"
	}
	for _, state := range []CheckState{CheckDisabled, CheckModified, CheckUntrusted} {
		if states[string(state)] {
			return state, "Codex reports this application's hooks as " + string(state), "Open Codex and run /hooks to review the hooks"
		}
	}
	return CheckOK, "", ""
}
