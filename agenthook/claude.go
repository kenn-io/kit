package agenthook

func claudeProfile() profileSpec {
	return newProfileSpec(
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
}
