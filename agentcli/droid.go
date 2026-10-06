package agentcli

import "fmt"

// NewDroid returns an adapter with the configured executable and arguments.
func NewDroid(command Command) (Adapter, error) {
	return newAdapter(Droid, command, "droid", droidCapabilities, buildDroid)
}

var droidCapabilities = Capabilities{
	Modes: []Mode{Interactive, NonInteractive}, Resume: true,
	OutputFormats: []OutputFormat{OutputText, OutputJSON, OutputJSONL}, Model: true,
	ReasoningLevels: []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	AutonomyLevels:  []AutonomyLevel{AutonomyLow, AutonomyMedium, AutonomyHigh},
	ApprovalModes:   []ApprovalMode{ApprovalBypass}, Tools: ToolCapabilities{AllowList: true, DenyList: true},
	DisableSkills: true,
}

func buildDroid(a *adapter, sessionID string, request Request) (Invocation, error) {
	if request.Mode == Interactive {
		return buildDroidInteractive(a, sessionID, request)
	}
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
	default:
	}
	stdin, err := stdinPrompt(request.Prompt)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Droid, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}

// buildDroidInteractive starts the Droid REPL. Factory documents only
// --resume and --disable-builtin-skills for it; the other controls are exec flags.
func buildDroidInteractive(a *adapter, sessionID string, request Request) (Invocation, error) {
	if err := rejectInteractivePrompt(Droid, request); err != nil {
		return Invocation{}, err
	}
	for _, control := range []struct {
		requested bool
		option    string
	}{
		{request.Model != "", "model"},
		{request.Reasoning != ReasoningDefault, "reasoning"},
		{request.Autonomy != AutonomyDefault, "autonomy"},
		{request.Approval != ApprovalDefault, "approval mode"},
		{len(request.AllowedTools) != 0 || len(request.DeniedTools) != 0, "tools"},
	} {
		if control.requested {
			return Invocation{}, unsupported(Droid, Interactive, control.option, "", "use noninteractive mode")
		}
	}
	args := a.base()
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if request.DisableSkills {
		args = append(args, "--disable-builtin-skills")
	}
	return Invocation{Argv: args}, nil
}
