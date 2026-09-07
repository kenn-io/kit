package agentcli

import "fmt"

// NewDroid returns a Factory Droid CLI adapter after validating configured
// global options. A zero Command uses "droid".
func NewDroid(command Command) (Adapter, error) {
	base, err := newAdapter(Droid, command, "droid", droidOptionGrammar)
	if err != nil {
		return nil, err
	}
	return &droidAdapter{adapter: base}, nil
}

type droidAdapter struct{ adapter }

var droidOptionGrammar = optionGrammar{
	"--disable-builtin-skills": flag("disable-builtin-skills"), "--append-system-prompt": value("append-system-prompt"),
	"--append-system-prompt-file": value("append-system-prompt-file"),
	"-m":                          value("model"), "--model": value("model"),
	"--auto": value("autonomy"), "--reasoning-effort": value("reasoning"),
	"--restrict-tools": value("allowed-tools"), "--disabled-tools": value("denied-tools"),
	"-o": value("output-format"), "--output-format": value("output-format"),
	"--skip-permissions-unsafe": flag("approval-bypass"),
	"-w":                        forbidden("worktree creation is a caller concern"), "--worktree": forbidden("worktree creation is a caller concern"),
	"--resume": forbidden("resume selects a session"), "-r": forbidden("resume has command-dependent meaning and is ambiguous in configured options"),
	"-s": forbidden("session selects a session"), "--session-id": forbidden("session selects a session"),
	"--fork": forbidden("fork changes session identity"),
	"-h":     forbidden("help is an action, not a launch option"), "--help": forbidden("help is an action, not a launch option"),
	"-v": forbidden("version is an action, not a launch option"), "--version": forbidden("version is an action, not a launch option"),
}

var droidCapabilities = Capabilities{
	Modes: []Mode{NonInteractive}, PromptSources: []PromptSource{PromptArgument, PromptStdin}, Resume: true,
	OutputFormats: []OutputFormat{OutputText, OutputJSON, OutputJSONL}, Model: true,
	ReasoningLevels: []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	AutonomyLevels:  []AutonomyLevel{AutonomyLow, AutonomyMedium, AutonomyHigh},
	ApprovalModes:   []ApprovalMode{ApprovalBypass}, Tools: ToolCapabilities{AllowList: true, DenyList: true},
	DisableSkills: true,
}

func (a *droidAdapter) Capabilities() Capabilities                { return cloneCapabilities(droidCapabilities) }
func (a *droidAdapter) Start(request Request) (Invocation, error) { return a.build("", request) }
func (a *droidAdapter) Resume(sessionID string, request Request) (Invocation, error) {
	sessionID, err := validateSessionID(sessionID)
	if err != nil {
		return Invocation{}, err
	}
	return a.build(sessionID, request)
}
func (a *droidAdapter) build(sessionID string, request Request) (Invocation, error) {
	mode, err := invocationMode(request.Mode)
	if err != nil {
		return Invocation{}, err
	}
	if mode != NonInteractive {
		return Invocation{}, unsupported(Droid, mode, "mode", string(mode), "request noninteractive mode")
	}
	if err := validateSupportedRequest(Droid, mode, request, droidCapabilities); err != nil {
		return Invocation{}, err
	}
	if err := validateDroidRequest(request); err != nil {
		return Invocation{}, err
	}
	checks := []struct {
		requested bool
		option    string
		hint      string
		names     []string
	}{
		{request.Model != "", "model", "remove the configured model or leave Request.Model empty", []string{"model"}},
		{request.Reasoning != ReasoningDefault, "reasoning", "remove the configured reasoning effort or leave Request.Reasoning empty", []string{"reasoning"}},
		{request.Autonomy != AutonomyDefault, "autonomy", "remove the configured autonomy level or leave Request.Autonomy empty", []string{"autonomy"}},
		{request.Approval != ApprovalDefault, "approval mode", "remove --skip-permissions-unsafe or leave Request.Approval empty", []string{"approval-bypass"}},
		{len(request.AllowedTools) != 0, "allowed tools", "remove configured restricted tools or leave Request.AllowedTools empty", []string{"allowed-tools"}},
		{len(request.DeniedTools) != 0, "denied tools", "remove configured disabled tools or leave Request.DeniedTools empty", []string{"denied-tools"}},
		{request.DisableSkills, "built-in skills", "remove --disable-builtin-skills or leave Request.DisableSkills false", []string{"disable-builtin-skills"}},
		{request.OutputFormat != OutputDefault, "output format", "remove the configured output format or leave Request.OutputFormat empty", []string{"output-format"}},
	}
	for _, check := range checks {
		if err := a.rejectsConfigured(check.requested, check.option, check.hint, check.names...); err != nil {
			return Invocation{}, err
		}
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
		args = append(args, "--reasoning-effort", droidReasoning(request.Reasoning))
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
	args, stdin, err := appendPrompt(args, request.Prompt, "", false)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Droid, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}
func validateDroidRequest(request Request) error {
	if request.Prompt.Source != PromptNone && request.Prompt.Source != PromptArgument && request.Prompt.Source != PromptStdin {
		return unsupported(Droid, NonInteractive, "prompt transport", string(request.Prompt.Source), "send the prompt as an argument or over stdin")
	}
	if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText && request.OutputFormat != OutputJSON && request.OutputFormat != OutputJSONL {
		return unsupported(Droid, NonInteractive, "output format", string(request.OutputFormat), "request text, json, or jsonl")
	}
	if request.Approval != ApprovalDefault && request.Approval != ApprovalBypass {
		return unsupported(Droid, NonInteractive, "approval mode", string(request.Approval), "request bypass or use the default")
	}
	if request.Autonomy != AutonomyDefault && request.Autonomy != AutonomyLow && request.Autonomy != AutonomyMedium && request.Autonomy != AutonomyHigh {
		return unsupported(Droid, NonInteractive, "autonomy", string(request.Autonomy), "request low, medium, or high")
	}
	if request.Reasoning != ReasoningDefault && droidReasoning(request.Reasoning) == "" {
		return unsupported(Droid, NonInteractive, "reasoning", string(request.Reasoning), "request low, medium, high, xhigh, or maximum")
	}
	if err := validateValues("allowed tools", request.AllowedTools); err != nil {
		return err
	}
	if err := validateValues("denied tools", request.DeniedTools); err != nil {
		return err
	}
	return nil
}
func droidReasoning(level ReasoningLevel) string {
	switch level {
	case ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh:
		return string(level)
	case ReasoningMaximum:
		return "max"
	default:
		return ""
	}
}
