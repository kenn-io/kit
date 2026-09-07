package agentcli

import "fmt"

// NewGemini returns a Gemini CLI adapter after validating configured options.
// A zero Command uses "gemini".
func NewGemini(command Command) (Adapter, error) {
	base, err := newAdapter(Gemini, command, "gemini", geminiOptionGrammar)
	if err != nil {
		return nil, err
	}
	return &geminiAdapter{adapter: base}, nil
}

type geminiAdapter struct{ adapter }

var geminiOptionGrammar = optionGrammar{
	"-d": flag("debug"), "--debug": flag("debug"), "-m": value("model"), "--model": value("model"),
	"--skip-trust": flag("skip-trust"), "--policy": value("policy"), "--admin-policy": value("admin-policy"),
	"--allowed-mcp-server-names": value("allowed-mcp-server-names"), "--allowed-tools": value("allowed-tools"),
	"-e": value("extensions"), "--extensions": value("extensions"), "--include-directories": value("include-directories"),
	"--screen-reader": flag("screen-reader"), "--raw-output": flag("raw-output"), "--accept-raw-output-risk": flag("accept-raw-output-risk"),
	"-p": forbidden("prompt belongs in Request.Prompt"), "--prompt": forbidden("prompt belongs in Request.Prompt"),
	"-i": forbidden("interactive prompt belongs in Request.Prompt"), "--prompt-interactive": forbidden("interactive prompt belongs in Request.Prompt"),
	"-r": forbidden("resume selects a session"), "--resume": forbidden("resume selects a session"),
	"--session-file": forbidden("session file selects a session"), "--session-id": forbidden("session identity belongs in Start or Resume"),
	"--list-sessions": forbidden("list-sessions is an action"), "--delete-session": forbidden("delete-session is an action"),
	"--acp": forbidden("ACP server mode is a different protocol"), "--experimental-acp": forbidden("ACP server mode is a different protocol"),
	"-w": forbidden("worktree creation is a caller concern"), "--worktree": forbidden("worktree creation is a caller concern"),
	"-s": forbidden("sandbox selection belongs in Request.Sandbox"), "--sandbox": forbidden("sandbox selection belongs in Request.Sandbox"),
	"-y": flag("approval-bypass"), "--yolo": flag("approval-bypass"),
	"--approval-mode": value("approval-mode"),
	"-o":              value("output-format"), "--output-format": value("output-format"),
	"-l": forbidden("list-extensions is an action"), "--list-extensions": forbidden("list-extensions is an action"),
	"-h": forbidden("help is an action, not a launch option"), "--help": forbidden("help is an action, not a launch option"),
	"-v": forbidden("version is an action, not a launch option"), "--version": forbidden("version is an action, not a launch option"),
}

var geminiCapabilities = Capabilities{
	Modes: []Mode{NonInteractive}, PromptSources: []PromptSource{PromptArgument, PromptStdin}, Resume: true,
	OutputFormats: []OutputFormat{OutputText, OutputJSON, OutputJSONL}, Model: true,
	ApprovalModes: []ApprovalMode{ApprovalNever, ApprovalBypass},
}

func (a *geminiAdapter) Capabilities() Capabilities                { return cloneCapabilities(geminiCapabilities) }
func (a *geminiAdapter) Start(request Request) (Invocation, error) { return a.build("", request) }
func (a *geminiAdapter) Resume(sessionID string, request Request) (Invocation, error) {
	sessionID, err := validateSessionID(sessionID)
	if err != nil {
		return Invocation{}, err
	}
	return a.build(sessionID, request)
}
func (a *geminiAdapter) build(sessionID string, request Request) (Invocation, error) {
	mode, err := invocationMode(request.Mode)
	if err != nil {
		return Invocation{}, err
	}
	if mode != NonInteractive {
		return Invocation{}, unsupported(Gemini, mode, "mode", string(mode), "request noninteractive mode")
	}
	if err := validateSupportedRequest(Gemini, mode, request, geminiCapabilities); err != nil {
		return Invocation{}, err
	}
	if err := validateGeminiRequest(request); err != nil {
		return Invocation{}, err
	}
	if err := a.rejectsConfigured(request.Model != "", "model", "remove the configured model or leave Request.Model empty", "model"); err != nil {
		return Invocation{}, err
	}
	if err := a.rejectsConfigured(request.OutputFormat != OutputDefault, "output format", "remove the configured output format or leave Request.OutputFormat empty", "output-format"); err != nil {
		return Invocation{}, err
	}
	if err := a.rejectsConfigured(request.Approval != ApprovalDefault, "approval mode", "remove the configured approval option or leave Request.Approval empty", "approval-mode", "approval-bypass"); err != nil {
		return Invocation{}, err
	}
	args := a.base()
	if request.OutputFormat == OutputJSON {
		args = append(args, "--output-format", "json")
	}
	if request.OutputFormat == OutputJSONL {
		args = append(args, "--output-format", "stream-json")
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	switch request.Approval {
	case ApprovalNever:
		args = append(args, "--approval-mode", "plan")
	case ApprovalBypass:
		args = append(args, "--approval-mode", "yolo")
	}
	if err := validatePrompt(request.Prompt); err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Gemini, err)
	}
	var stdin *string
	switch request.Prompt.Source {
	case PromptArgument:
		args = append(args, "--prompt", request.Prompt.Text)
	case PromptStdin:
		args = append(args, "--prompt", "")
		stdin = new(request.Prompt.Text)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}
func validateGeminiRequest(request Request) error {
	if request.Prompt.Source != PromptNone && request.Prompt.Source != PromptArgument && request.Prompt.Source != PromptStdin {
		return unsupported(Gemini, NonInteractive, "prompt transport", string(request.Prompt.Source), "send the prompt with --prompt or over stdin")
	}
	if len(request.Prompt.Files) != 0 {
		return unsupported(Gemini, NonInteractive, "prompt files", "", "include file references in the prompt text")
	}
	if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText && request.OutputFormat != OutputJSON && request.OutputFormat != OutputJSONL {
		return unsupported(Gemini, NonInteractive, "output format", string(request.OutputFormat), "request text, json, or jsonl")
	}
	if request.Approval != ApprovalDefault && request.Approval != ApprovalNever && request.Approval != ApprovalBypass {
		return unsupported(Gemini, NonInteractive, "approval mode", string(request.Approval), "request never, bypass, or use the default")
	}
	if request.Sandbox != SandboxDefault {
		return unsupported(Gemini, NonInteractive, "sandbox", string(request.Sandbox), "Gemini's boolean sandbox flag does not map to a portable sandbox mode")
	}
	return nil
}
