package agentcli

import "fmt"

// NewDroid returns an adapter with the configured executable and arguments.
func NewDroid(command Command) (Adapter, error) {
	return newAdapter(Droid, command, "droid", droidCapabilities, buildDroid)
}

var droidCapabilities = Capabilities{
	Modes: []Mode{NonInteractive}, Resume: true,
	OutputFormats: []OutputFormat{OutputText, OutputJSON, OutputJSONL}, Model: true,
	ReasoningLevels: []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	AutonomyLevels:  []AutonomyLevel{AutonomyLow, AutonomyMedium, AutonomyHigh},
	ApprovalModes:   []ApprovalMode{ApprovalBypass}, Tools: ToolCapabilities{AllowList: true, DenyList: true},
	DisableSkills: true,
}

func buildDroid(a *adapter, sessionID string, request Request) (Invocation, error) {
	args := []string{a.executable, "exec"}
	args = append(args, a.options...)
	if sessionID != "" {
		args = append(args, "--session-id", sessionID)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Reasoning != ReasoningDefault {
		args = append(args, "--reasoning-effort", reasoningValue(request.Reasoning))
	}
	if request.Autonomy != AutonomyDefault {
		args = append(args, "--auto", string(request.Autonomy))
	}
	if request.Approval == ApprovalBypass {
		args = append(args, "--skip-permissions-unsafe")
	}
	if len(request.AllowedTools) != 0 {
		args = append(args, "--restrict-tools", joinComma(request.AllowedTools))
	}
	if len(request.DeniedTools) != 0 {
		args = append(args, "--disabled-tools", joinComma(request.DeniedTools))
	}
	if request.DisableSkills {
		args = append(args, "--disable-builtin-skills")
	}
	switch request.OutputFormat {
	case OutputJSON:
		args = append(args, "--output-format", "json")
	case OutputJSONL:
		args = append(args, "--output-format", "stream-json")
	}
	stdin, err := stdinPrompt(request.Prompt)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Droid, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}
