package agenthook

func droidProfile() profileSpec {
	return newProfileSpec(
		Profile{
			Agent: AgentDroid, DisplayName: "Factory Droid",
			ConfigFilename: "hooks.json",
			SupportedEvents: []Event{
				EventPreToolUse, EventPostToolUse, EventStop,
			},
		},
		formatNestedJSON,
		"Execute",
		func() (string, error) { return userDotDir(".factory") },
	)
}
