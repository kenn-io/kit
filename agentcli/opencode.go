package agentcli

import "fmt"

// NewOpenCode returns an adapter with the configured executable and arguments.
func NewOpenCode(command Command) (Adapter, error) {
	return newAdapter(OpenCode, command, "opencode", openCodeCapabilities, buildOpenCode)
}

var openCodeCapabilities = Capabilities{
	Modes:         []Mode{Interactive, NonInteractive},
	Resume:        true,
	OutputFormats: []OutputFormat{OutputText, OutputJSONL},
	Model:         true,
}

func buildOpenCode(a *adapter, sessionID string, request Request) (Invocation, error) {
	if request.Mode == Interactive {
		if err := rejectInteractivePrompt(OpenCode, request); err != nil {
			return Invocation{}, err
		}
		// The 2.x TUI takes --session but no --model (opencode --help, v2.0.14).
		if request.Model != "" {
			return Invocation{}, unsupported(OpenCode, Interactive, "model", "", "use noninteractive mode")
		}
		args := a.base()
		if sessionID != "" {
			args = append(args, "--session", sessionID)
		}
		return Invocation{Argv: args}, nil
	}
	args := append(a.base(), "run")
	if request.OutputFormat == OutputJSONL {
		args = append(args, "--format", "json")
	}
	if sessionID != "" {
		args = append(args, "--session", sessionID)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	stdin, err := stdinPrompt(request.Prompt)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", OpenCode, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}
