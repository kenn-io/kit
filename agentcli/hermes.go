package agentcli

// NewHermes returns an adapter with the configured executable and arguments.
func NewHermes(command Command) (Adapter, error) {
	return newAdapter(Hermes, command, "hermes", hermesCapabilities, buildHermes)
}

var hermesCapabilities = Capabilities{
	Modes: []Mode{Interactive}, Resume: true,
	OutputFormats: []OutputFormat{OutputText},
}

func buildHermes(a *adapter, sessionID string, request Request) (Invocation, error) {
	if err := rejectInteractivePrompt(Hermes, request); err != nil {
		return Invocation{}, err
	}
	args := a.base()
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	return Invocation{Argv: args}, nil
}
