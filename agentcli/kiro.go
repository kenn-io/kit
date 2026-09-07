package agentcli

import "fmt"

// NewKiro returns a Kiro CLI adapter after validating configured global
// options. A zero Command uses "kiro-cli".
func NewKiro(command Command) (Adapter, error) {
	base, err := newAdapter(Kiro, command, "kiro-cli", kiroOptionGrammar)
	if err != nil {
		return nil, err
	}
	return &kiroAdapter{adapter: base}, nil
}

type kiroAdapter struct{ adapter }

var kiroOptionGrammar = optionGrammar{
	"--verbose": flag("verbose"), "-v": flag("verbose"), "--agent": value("agent"),
	"--require-mcp-startup": flag("require-mcp-startup"), "--wrap": value("wrap"),
	"--no-interactive": forbidden("mode belongs in Request.Mode"),
	"--resume":         forbidden("resume selects a session"), "-r": forbidden("resume selects a session"),
	"--resume-picker": forbidden("resume-picker selects a session"), "--resume-id": forbidden("resume-id selects a session"),
	"--list-sessions": forbidden("list-sessions is an action"), "--delete-session": forbidden("delete-session is an action"),
	"--list-models":     forbidden("list-models is an action"),
	"--trust-all-tools": flag("approval-bypass"), "--trust-tools": value("allowed-tools"),
	"--effort": value("reasoning"),
	"-h":       forbidden("help is an action, not a launch option"), "--help": forbidden("help is an action, not a launch option"),
	"-V": forbidden("version is an action, not a launch option"), "--version": forbidden("version is an action, not a launch option"),
}

var kiroCapabilities = Capabilities{
	Modes:           []Mode{NonInteractive},
	PromptSources:   []PromptSource{PromptArgument},
	Resume:          true,
	OutputFormats:   []OutputFormat{OutputText},
	ReasoningLevels: []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	ApprovalModes:   []ApprovalMode{ApprovalBypass},
	Tools:           ToolCapabilities{AllowList: true},
}

func (a *kiroAdapter) Capabilities() Capabilities                { return cloneCapabilities(kiroCapabilities) }
func (a *kiroAdapter) Start(request Request) (Invocation, error) { return a.build("", request) }
func (a *kiroAdapter) Resume(sessionID string, request Request) (Invocation, error) {
	sessionID, err := validateSessionID(sessionID)
	if err != nil {
		return Invocation{}, err
	}
	return a.build(sessionID, request)
}
func (a *kiroAdapter) build(sessionID string, request Request) (Invocation, error) {
	mode, err := invocationMode(request.Mode)
	if err != nil {
		return Invocation{}, err
	}
	if mode != NonInteractive {
		return Invocation{}, unsupported(Kiro, mode, "mode", string(mode), "request noninteractive mode")
	}
	if err := validateSupportedRequest(Kiro, mode, request, kiroCapabilities); err != nil {
		return Invocation{}, err
	}
	if err := validateKiroRequest(request); err != nil {
		return Invocation{}, err
	}
	if err := a.rejectsConfigured(request.Reasoning != ReasoningDefault, "reasoning", "remove the configured effort or leave Request.Reasoning empty", "reasoning"); err != nil {
		return Invocation{}, err
	}
	if err := a.rejectsConfigured(request.Approval != ApprovalDefault, "approval mode", "remove --trust-all-tools or leave Request.Approval empty", "approval-bypass"); err != nil {
		return Invocation{}, err
	}
	if err := a.rejectsConfigured(len(request.AllowedTools) != 0, "allowed tools", "remove configured trusted tools or leave Request.AllowedTools empty", "allowed-tools"); err != nil {
		return Invocation{}, err
	}
	args := []string{a.executable, "chat"}
	args = append(args, a.options...)
	args = append(args, "--no-interactive")
	if sessionID != "" {
		args = append(args, "--resume-id", sessionID)
	}
	if request.Reasoning != ReasoningDefault {
		args = append(args, "--effort", kiroReasoning(request.Reasoning))
	}
	if request.Approval == ApprovalBypass {
		args = append(args, "--trust-all-tools")
	}
	if len(request.AllowedTools) != 0 {
		args = append(args, "--trust-tools", joinComma(request.AllowedTools))
	}
	if request.Prompt.Source == PromptArgument {
		if err := validatePrompt(request.Prompt); err != nil {
			return Invocation{}, err
		}
		args = append(args, "--")
		args, _, err = appendPrompt(args, request.Prompt, "", false)
		if err != nil {
			return Invocation{}, fmt.Errorf("build %s invocation: %w", Kiro, err)
		}
		return Invocation{Argv: args}, nil
	}
	return Invocation{Argv: args}, nil
}
func validateKiroRequest(request Request) error {
	if request.Prompt.Source != PromptNone && request.Prompt.Source != PromptArgument {
		return unsupported(Kiro, NonInteractive, "prompt transport", string(request.Prompt.Source), "send the prompt as an argument")
	}
	if len(request.Prompt.Files) != 0 {
		return unsupported(Kiro, NonInteractive, "prompt files", "", "include file references in the prompt text")
	}
	if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText {
		return unsupported(Kiro, NonInteractive, "output format", string(request.OutputFormat), "request text")
	}
	if request.Approval != ApprovalDefault && request.Approval != ApprovalBypass {
		return unsupported(Kiro, NonInteractive, "approval mode", string(request.Approval), "request bypass or use the default")
	}
	if request.Reasoning != ReasoningDefault && kiroReasoning(request.Reasoning) == "" {
		return unsupported(Kiro, NonInteractive, "reasoning", string(request.Reasoning), "request low, medium, high, xhigh, or maximum")
	}
	if err := validateValues("allowed tools", request.AllowedTools); err != nil {
		return err
	}
	return nil
}
func kiroReasoning(level ReasoningLevel) string {
	switch level {
	case ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh:
		return string(level)
	case ReasoningMaximum:
		return "max"
	default:
		return ""
	}
}
