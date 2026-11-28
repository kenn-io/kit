package agenthook

import "path/filepath"

// agyProfile describes the Antigravity CLI (agy). Its global hooks file maps
// user-chosen hook names to event registrations:
// https://antigravity.google/docs/hooks/
func agyProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentAgy, DisplayName: "Antigravity CLI",
			ConfigFilename: "hooks.json",
			// PostToolUse is left out: agy's PostToolUse payload carries no
			// tool result, which the Claude contract requires.
			SupportedEvents: []Event{
				EventSessionStart, EventUserPromptSubmit, EventPreToolUse, EventStop,
			},
		},
		formatAgyJSON,
		"run_command",
		func() (string, error) {
			dir, err := userDotDir(".gemini")
			return filepath.Join(dir, "config"), err
		},
	)
	spec.eventName = agyEventName
	// agy decision fields are required on PreToolUse and Stop, and prompt
	// control uses PreInvocation's terminationBehavior:
	// https://antigravity.google/docs/hooks/
	spec.responseFormat = responseAgy
	// SessionStart has no source, and PreInvocation's lastUserInput is the
	// prompt only when the turn came from the user.
	spec.sessionSourceRequirement = inputOptional
	spec.userPromptRequirement = inputOptional
	return spec
}

// agyEventName maps UserPromptSubmit to PreInvocation, which agy runs before
// every model call in a turn, so handlers see it more than once per prompt.
func agyEventName(event Event) string {
	if event == EventUserPromptSubmit {
		return "PreInvocation"
	}
	return string(event)
}
