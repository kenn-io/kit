package agenthook

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v7"

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
func checkCodex(ctx context.Context, path string, owned []ownedRegistration) (CheckState, string, string) {
	const retryPause = 500 * time.Millisecond
	const addedWaitBudget = 10 * time.Second
	var state CheckState
	var reason, step string
	attempt := 0
	_, retryErr := backoff.Retry(ctx, func() (struct{}, error) {
		if err := ctx.Err(); err != nil {
			state, reason, step = codexFailure(err, "Check Codex app-server startup and retry")
			return struct{}{}, backoff.Permanent(err)
		}
		attempt++
		attemptCtx := ctx
		cancel := func() {}
		if attempt == 2 {
			// The pause and second attempt together add at most ten seconds.
			attemptCtx, cancel = context.WithTimeout(ctx, addedWaitBudget-retryPause)
		}
		defer cancel()
		var err error
		state, reason, step, err = checkCodexAttempt(attemptCtx, path, owned)
		if err == nil {
			return struct{}{}, nil
		}
		if !transientCodexError(err) {
			return struct{}{}, backoff.Permanent(err)
		}
		return struct{}{}, err
	}, backoff.WithBackOff(backoff.NewConstantBackOff(retryPause)), backoff.WithMaxTries(2), backoff.WithMaxElapsedTime(0))
	if err := ctx.Err(); retryErr != nil && err != nil {
		state, reason, step = codexFailure(err, step)
	}
	return state, reason, step
}

func transientCodexError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.EAGAIN) {
		return true
	}
	var pathErr *os.PathError
	return errors.As(err, &pathErr) && !errors.Is(err, os.ErrPermission) && (pathErr.Op == "read" || pathErr.Op == "write")
}

func checkCodexAttempt(ctx context.Context, path string, owned []ownedRegistration) (CheckState, string, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return CheckUnknown, err.Error(), "Choose a working directory for Codex", nil
	}
	client, err := codexapp.Start(ctx, "codex", cwd)
	if err != nil {
		state, reason, step := codexFailure(err, "Check Codex app-server startup and retry")
		return state, reason, step, err
	}
	defer client.Close()
	var config struct {
		Config *struct {
			Features map[string]*bool `json:"features"`
		} `json:"config"`
	}
	if err := client.Call("config/read", map[string]any{"includeLayers": false, "cwd": cwd}, &config); err != nil {
		state, reason, step := codexFailure(err, "Check Codex config and retry")
		return state, reason, step, err
	}
	if config.Config == nil {
		return CheckUnknown, "Codex didn't report whether hooks are enabled for this workspace", "Check Codex config and retry", nil
	}
	if enabled := config.Config.Features["hooks"]; enabled != nil && !*enabled {
		return CheckDisabled, "Codex hooks are turned off for this workspace", "Set features.hooks to true in the Codex config that applies to this workspace", nil
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
		state, reason, step := codexFailure(err, "Check hooks in Codex and retry")
		return state, reason, step, err
	}
	if len(listed.Data) != 1 {
		return CheckUnknown, "Codex didn't report hooks for this folder", "Check hooks in Codex and retry", nil
	}
	entry := listed.Data[0]
	if len(entry.Errors) > 0 {
		return CheckUnknown, fmt.Sprintf("Codex reported errors while loading hooks: %s", entry.Errors), "Fix the reported hook errors", nil
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
				return CheckUnknown, "Codex reported unknown hook trust state: " + hook.TrustStatus, "Review hooks in Codex with /hooks", nil
			}
		}
	}
	if count != len(owned) {
		return CheckUnknown, "Codex didn't list all of this application's installed hooks", "Open Codex and run /hooks to review the hooks", nil
	}
	for _, state := range []CheckState{CheckDisabled, CheckModified, CheckUntrusted} {
		if states[string(state)] {
			return state, "Codex says these hooks need review: " + string(state), "Open Codex and run /hooks to review the hooks", nil
		}
	}
	return CheckOK, "", "", nil
}

func codexFailure(err error, step string) (CheckState, string, string) {
	if errors.Is(err, exec.ErrNotFound) {
		step = "Install Codex and make it available on PATH"
	}
	return CheckUnknown, err.Error(), step
}
