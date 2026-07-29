package agenthook

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
	return spec
}
