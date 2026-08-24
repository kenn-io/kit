package agenthook

import (
	"os"
	"path/filepath"
	"strings"
)

func openCodeProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentOpenCode, DisplayName: "OpenCode",
			ConfigEnvironment: "XDG_CONFIG_HOME",
			ConfigFilename:    filepath.Join("plugins", "roborev-agent-hook.js"),
			SupportedEvents: []Event{
				EventUserPromptSubmit, EventPreToolUse, EventPostToolUse,
			},
		},
		formatOpenCodePlugin,
		"bash",
		openCodeDefaultDir,
	)
	spec.configEnvSubdir = "opencode"
	spec.eventName = openCodeEventName
	spec.responseFormat = responseOpenCode
	spec.sessionSourceRequirement = inputOptional
	spec.sessionEndReasonRequirement = inputOptional
	return spec
}

func openCodeDefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "opencode"), nil
}

func openCodeEventName(event Event) string {
	switch event {
	case EventUserPromptSubmit:
		return "chat.message"
	case EventPreToolUse:
		return "tool.execute.before"
	case EventPostToolUse:
		return "tool.execute.after"
	default:
		return strings.ToLower(string(event))
	}
}
