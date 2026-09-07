package agentcli

import "fmt"

// NewKilo returns a Kilo adapter after validating configured global options.
// A zero Command uses "kilo".
func NewKilo(command Command) (Adapter, error) {
	base, err := newAdapter(Kilo, command, "kilo", kiloOptionGrammar)
	if err != nil {
		return nil, err
	}
	return &kiloAdapter{adapter: base}, nil
}

type kiloAdapter struct{ adapter }

var kiloOptionGrammar = optionGrammar{
	"--print-logs": flag("print-logs"), "--log-level": value("log-level"),
	"-m": value("model"), "--model": value("model"), "--agent": value("agent"),
	"-c": forbidden("continue selects a session"), "--continue": forbidden("continue selects a session"),
	"-s": forbidden("session selects a session"), "--session": forbidden("session selects a session"),
	"--fork": forbidden("fork changes session identity"), "--cloud-fork": forbidden("cloud-fork changes session identity"),
	"--prompt": forbidden("prompt belongs in Request.Prompt"), "--auto": forbidden("automatic permission approval belongs in Request.Approval"),
	"-h": forbidden("help is an action, not a launch option"), "--help": forbidden("help is an action, not a launch option"),
	"-v": forbidden("version is an action, not a launch option"), "--version": forbidden("version is an action, not a launch option"),
}

var kiloCapabilities = Capabilities{
	Modes:           []Mode{NonInteractive},
	PromptSources:   []PromptSource{PromptStdin},
	Resume:          true,
	OutputFormats:   []OutputFormat{OutputText, OutputJSONL},
	Model:           true,
	ReasoningLevels: []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	ApprovalModes:   []ApprovalMode{ApprovalBypass},
}

func (a *kiloAdapter) Capabilities() Capabilities                { return cloneCapabilities(kiloCapabilities) }
func (a *kiloAdapter) Start(request Request) (Invocation, error) { return a.build("", request) }
func (a *kiloAdapter) Resume(sessionID string, request Request) (Invocation, error) {
	sessionID, err := validateSessionID(sessionID)
	if err != nil {
		return Invocation{}, err
	}
	return a.build(sessionID, request)
}
func (a *kiloAdapter) build(sessionID string, request Request) (Invocation, error) {
	mode, err := invocationMode(request.Mode)
	if err != nil {
		return Invocation{}, err
	}
	if mode != NonInteractive {
		return Invocation{}, unsupported(Kilo, mode, "mode", string(mode), "request noninteractive mode")
	}
	if err := validateSupportedRequest(Kilo, mode, request, kiloCapabilities); err != nil {
		return Invocation{}, err
	}
	if err := validateKiloRequest(request); err != nil {
		return Invocation{}, err
	}
	if err := a.rejectsConfigured(request.Model != "", "model", "remove the configured model or leave Request.Model empty", "model"); err != nil {
		return Invocation{}, err
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
	if request.Approval == ApprovalBypass {
		args = append(args, "--auto")
	}
	if variant := kiloReasoning(request.Reasoning); variant != "" {
		args = append(args, "--variant", variant)
	}
	args, stdin, err := appendPrompt(args, request.Prompt, "", false)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Kilo, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}
func validateKiloRequest(request Request) error {
	if request.Prompt.Source != PromptNone && request.Prompt.Source != PromptStdin {
		return unsupported(Kilo, NonInteractive, "prompt transport", string(request.Prompt.Source), "send the prompt over stdin")
	}
	if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText && request.OutputFormat != OutputJSONL {
		return unsupported(Kilo, NonInteractive, "output format", string(request.OutputFormat), "request text or jsonl")
	}
	if request.Approval != ApprovalDefault && request.Approval != ApprovalBypass {
		return unsupported(Kilo, NonInteractive, "approval mode", string(request.Approval), "request bypass or use the default")
	}
	if request.Reasoning != ReasoningDefault && kiloReasoning(request.Reasoning) == "" {
		return unsupported(Kilo, NonInteractive, "reasoning", string(request.Reasoning), "request low, high, xhigh, or maximum")
	}
	return nil
}
func kiloReasoning(level ReasoningLevel) string {
	switch level {
	case ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh:
		return string(level)
	case ReasoningMaximum:
		return "max"
	default:
		return ""
	}
}
