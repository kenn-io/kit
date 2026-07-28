package agenthook

import "time"

func qwenProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentQwen, DisplayName: "Qwen Code",
			ConfigEnvironment: "QWEN_HOME", ConfigFilename: "settings.json",
			SupportedEvents: []Event{
				EventSessionStart, EventUserPromptSubmit, EventPreToolUse,
				EventPostToolUse, EventPostToolUseFailure, EventPermissionRequest,
				EventNotification, EventStop, EventSessionEnd,
			},
		},
		formatNestedJSON,
		"run_shell_command",
		func() (string, error) { return userDotDir(".qwen") },
	)
	spec.timeoutUnit = time.Millisecond
	return spec
}
