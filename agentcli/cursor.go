package agentcli

import "fmt"

// NewCursor returns an adapter with the configured executable and arguments.
func NewCursor(command Command) (Adapter, error) {
	return newAdapter(Cursor, command, "agent", cursorCapabilities, buildCursor)
}

var cursorCapabilities = Capabilities{
	Modes: []Mode{NonInteractive}, Resume: true,
	OutputFormats: []OutputFormat{OutputText, OutputJSON, OutputJSONL}, Model: true,
	ApprovalModes: []ApprovalMode{ApprovalNever, ApprovalBypass},
}

func buildCursor(a *adapter, sessionID string, request Request) (Invocation, error) {
	args := append(a.base(), "--print")
	if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText {
		format := string(request.OutputFormat)
		if request.OutputFormat == OutputJSONL {
			format = "stream-json"
		}
		args = append(args, "--output-format", format)
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	switch request.Approval {
	case ApprovalNever:
		args = append(args, "--mode", "plan")
	case ApprovalBypass:
		args = append(args, "--force")
	}
	stdin, err := stdinPrompt(request.Prompt)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Cursor, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}
