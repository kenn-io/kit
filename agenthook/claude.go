package agenthook

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitrepo "go.kenn.io/kit/git/repo"
)

func claudeProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentClaude, DisplayName: "Claude Code",
			ConfigEnvironment: "CLAUDE_CONFIG_DIR", ConfigFilename: "settings.json",
			SupportedEvents: []Event{
				EventSessionStart, EventUserPromptSubmit, EventPreToolUse,
				EventPostToolUse, EventPostToolUseFailure, EventPermissionRequest,
				EventNotification, EventStop, EventSessionEnd,
			},
		},
		formatNestedJSON,
		"Bash",
		func() (string, error) { return userDotDir(".claude") },
	)
	// Claude defines the package's public response vocabulary:
	// https://code.claude.com/docs/en/hooks#json-output
	spec.responseFormat = responseClaude
	// Claude SessionStart and SessionEnd require source and reason:
	// https://code.claude.com/docs/en/hooks#hook-inputs
	spec.sessionSourceRequirement = inputRequired
	spec.sessionEndReasonRequirement = inputRequired
	// Windows Claude runs a command string through Git Bash, which strips
	// backslashes, or PowerShell; the exec form ("command" plus "args") runs the
	// executable directly: https://code.claude.com/docs/en/hooks#command-hook-fields
	spec.windowsCommandStyle = windowsCommandExec
	return spec
}

// https://code.claude.com/docs/en/settings#settings-precedence
func claudeHooksDisabled(ctx context.Context, user map[string]any) (bool, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return false, err
	}
	paths := []string{filepath.Join(cwd, ".claude", "settings.json"), filepath.Join(cwd, ".claude", "settings.local.json")}
	if runtime.GOOS != "windows" {
		// Claude keeps local settings at the main checkout root on Unix.
		// https://code.claude.com/docs/en/settings#where-claude-code-keeps-the-local-file-in-a-git-repository
		root, err := gitrepo.MainRoot(ctx, cwd)
		if err != nil && !gitcmd.IsExitCode(err, 128) && !errors.Is(err, exec.ErrNotFound) {
			return false, err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return false, err
		}
		if root != "" && !sameConfigPath(root, cwd) && !sameConfigPath(root, home) {
			paths = append(paths, filepath.Join(root, ".claude", "settings.local.json"))
		}
	}
	layers := []map[string]any{user}
	for _, path := range paths {
		settings, _, err := readJSONConfig(path)
		if err != nil {
			return false, err
		}
		layers = append(layers, settings)
	}
	disabled := false
	for _, settings := range layers {
		if value, exists := settings["disableAllHooks"]; exists {
			var ok bool
			disabled, ok = value.(bool)
			if !ok {
				return false, errors.New("Claude Code setting disableAllHooks must be a boolean")
			}
		}
	}
	return disabled, nil
}
