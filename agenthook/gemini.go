package agenthook

import "time"

func geminiProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentGemini, DisplayName: "Gemini CLI",
			ConfigEnvironment: "GEMINI_CLI_HOME", ConfigFilename: "settings.json",
			SupportedEvents: []Event{
				EventSessionStart, EventUserPromptSubmit, EventPreToolUse,
				EventPostToolUse, EventNotification, EventStop, EventSessionEnd,
			},
		},
		formatNestedJSON,
		"run_shell_command",
		func() (string, error) { return userDotDir(".gemini") },
	)
	spec.configEnvSubdir = ".gemini"
	spec.eventName = geminiEventName
	spec.timeoutUnit = time.Millisecond
	return spec
}

func geminiEventName(event Event) string {
	switch event {
	case EventUserPromptSubmit:
		return "BeforeAgent"
	case EventPreToolUse:
		return "BeforeTool"
	case EventPostToolUse:
		return "AfterTool"
	case EventStop:
		return "AfterAgent"
	default:
		return string(event)
	}
}
