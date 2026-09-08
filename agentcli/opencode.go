package agentcli

import "fmt"

// NewOpenCode returns an adapter with the configured executable and arguments.
func NewOpenCode(command Command) (Adapter, error) {
	return newAdapter(OpenCode, command, "opencode", openCodeCapabilities, buildOpenCode)
}

var openCodeCapabilities = Capabilities{
	Modes:         []Mode{NonInteractive},
	Resume:        true,
	OutputFormats: []OutputFormat{OutputText, OutputJSONL},
	Model:         true,
}

func buildOpenCode(a *adapter, sessionID string, request Request) (Invocation, error) {
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
