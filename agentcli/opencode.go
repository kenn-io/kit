package agentcli

import "fmt"

// NewOpenCode returns an OpenCode adapter after validating configured global
// options. A zero Command uses "opencode".
func NewOpenCode(command Command) (Adapter, error) {
	base, err := newAdapter(OpenCode, command, "opencode", openCodeOptionGrammar)
	if err != nil {
		return nil, err
	}
	return &openCodeAdapter{adapter: base}, nil
}

type openCodeAdapter struct{ adapter }

var openCodeOptionGrammar = optionGrammar{
	"--print-logs": flag("print-logs"), "--log-level": value("log-level"),
	"--pure": flag("pure"), "-m": value("model"), "--model": value("model"),
	"--agent": value("agent"),
	"-c":      forbidden("continue selects a session"), "--continue": forbidden("continue selects a session"),
	"-s": forbidden("session selects a session"), "--session": forbidden("session selects a session"),
	"--fork": forbidden("fork changes session identity"), "--prompt": forbidden("prompt belongs in Request.Prompt"),
	"--auto": forbidden("automatic permission approval belongs in Request.Approval"),
	"-h":     forbidden("help is an action, not a launch option"), "--help": forbidden("help is an action, not a launch option"),
	"-v": forbidden("version is an action, not a launch option"), "--version": forbidden("version is an action, not a launch option"),
}

var openCodeCapabilities = Capabilities{
	Modes:         []Mode{NonInteractive},
	PromptSources: []PromptSource{PromptStdin},
	Resume:        true,
	OutputFormats: []OutputFormat{OutputText, OutputJSONL},
	Model:         true,
}

func (a *openCodeAdapter) Capabilities() Capabilities { return cloneCapabilities(openCodeCapabilities) }
func (a *openCodeAdapter) Start(request Request) (Invocation, error) {
	return a.build("", request)
}
func (a *openCodeAdapter) Resume(sessionID string, request Request) (Invocation, error) {
	sessionID, err := validateSessionID(sessionID)
	if err != nil {
		return Invocation{}, err
	}
	return a.build(sessionID, request)
}

func (a *openCodeAdapter) build(sessionID string, request Request) (Invocation, error) {
	mode, err := invocationMode(request.Mode)
	if err != nil {
		return Invocation{}, err
	}
	if mode != NonInteractive {
		return Invocation{}, unsupported(OpenCode, mode, "mode", string(mode), "request noninteractive mode")
	}
	if err := validateSupportedRequest(OpenCode, mode, request, openCodeCapabilities); err != nil {
		return Invocation{}, err
	}
	if err := validateOpenCodeRequest(request); err != nil {
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
	args, stdin, err := appendPrompt(args, request.Prompt, "", false)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", OpenCode, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}

func validateOpenCodeRequest(request Request) error {
	if request.Prompt.Source != PromptNone && request.Prompt.Source != PromptStdin {
		return unsupported(OpenCode, NonInteractive, "prompt transport", string(request.Prompt.Source), "send the prompt over stdin")
	}
	if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText && request.OutputFormat != OutputJSONL {
		return unsupported(OpenCode, NonInteractive, "output format", string(request.OutputFormat), "request text or jsonl")
	}
	return nil
}
