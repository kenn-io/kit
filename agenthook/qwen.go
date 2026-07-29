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
		// Qwen hook matchers use the internal tool id run_shell_command, not
		// the Bash permission/display alias:
		// https://github.com/QwenLM/qwen-code/blob/main/docs/users/features/hooks.md#hook-configuration
		// https://github.com/QwenLM/qwen-code/blob/main/packages/core/src/tools/tool-names.ts#L20-L28
		"run_shell_command",
		func() (string, error) { return userDotDir(".qwen") },
	)
	spec.timeoutUnit = time.Millisecond
	return spec
}
