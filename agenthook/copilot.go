package agenthook

import "path/filepath"

func copilotProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentCopilot, DisplayName: "GitHub Copilot CLI",
			ConfigEnvironment: "COPILOT_HOME",
			ConfigFilename:    filepath.Join("hooks", "agenthook.json"),
			SupportedEvents: []Event{
				EventSessionStart, EventUserPromptSubmit, EventPreToolUse,
				EventPostToolUse, EventPostToolUseFailure, EventPermissionRequest,
				EventNotification, EventStop, EventSessionEnd,
			},
		},
		formatDirectJSON,
		"Bash",
		func() (string, error) { return userDotDir(".copilot") },
	)
	spec.windowsCommandStyle = windowsCommandPowerShell
	spec.timeoutField = "timeoutSec"
	spec.requireVersion = true
	return spec
}
