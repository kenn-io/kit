package agentcli

import "fmt"

// NewCursor returns a Cursor Agent adapter after validating configured
// options. A zero Command uses "agent".
func NewCursor(command Command) (Adapter, error) {
	base, err := newAdapter(Cursor, command, "agent", cursorOptionGrammar)
	if err != nil {
		return nil, err
	}
	return &cursorAdapter{adapter: base}, nil
}

type cursorAdapter struct{ adapter }

var cursorOptionGrammar = optionGrammar{
	"--api-key": value("api-key"), "-H": value("header"), "--header": value("header"),
	"--stream-partial-output": flag("stream-partial-output"), "--model": value("model"),
	"--sandbox": value("sandbox"), "--approve-mcps": flag("approve-mcps"), "--trust": flag("trust"),
	"--workspace": value("workspace"), "--add-dir": value("add-dir"), "--plugin-dir": value("plugin-dir"),
	"-p": forbidden("print mode is selected by Request.Mode"), "--print": forbidden("print mode is selected by Request.Mode"),
	"--output-format": forbidden("output format belongs in Request.OutputFormat"), "--mode": forbidden("execution mode belongs in Request.Approval"),
	"--plan": forbidden("execution mode belongs in Request.Approval"), "-f": forbidden("permission bypass belongs in Request.Approval"),
	"--force": forbidden("permission bypass belongs in Request.Approval"), "--yolo": forbidden("permission bypass belongs in Request.Approval"),
	"--resume": forbidden("resume selects a session"), "--continue": forbidden("continue selects a session"),
	"--list-models": forbidden("list-models is an action, not a launch option"),
	"-w":            forbidden("worktree creation is a caller concern"), "--worktree": forbidden("worktree creation is a caller concern"),
	"--worktree-base": forbidden("worktree creation is a caller concern"), "--skip-worktree-setup": forbidden("worktree creation is a caller concern"),
	"-h": forbidden("help is an action, not a launch option"), "--help": forbidden("help is an action, not a launch option"),
	"-v": forbidden("version is an action, not a launch option"), "--version": forbidden("version is an action, not a launch option"),
}

var cursorCapabilities = Capabilities{
	Modes: []Mode{NonInteractive}, PromptSources: []PromptSource{PromptStdin}, Resume: true,
	OutputFormats: []OutputFormat{OutputText, OutputJSON, OutputJSONL}, Model: true,
	ApprovalModes: []ApprovalMode{ApprovalNever, ApprovalBypass},
}

func (a *cursorAdapter) Capabilities() Capabilities                { return cloneCapabilities(cursorCapabilities) }
func (a *cursorAdapter) Start(request Request) (Invocation, error) { return a.build("", request) }
func (a *cursorAdapter) Resume(sessionID string, request Request) (Invocation, error) {
	sessionID, err := validateSessionID(sessionID)
	if err != nil {
		return Invocation{}, err
	}
	return a.build(sessionID, request)
}
func (a *cursorAdapter) build(sessionID string, request Request) (Invocation, error) {
	mode, err := invocationMode(request.Mode)
	if err != nil {
		return Invocation{}, err
	}
	if mode != NonInteractive {
		return Invocation{}, unsupported(Cursor, mode, "mode", string(mode), "request noninteractive mode")
	}
	if err := validateSupportedRequest(Cursor, mode, request, cursorCapabilities); err != nil {
		return Invocation{}, err
	}
	if err := validateCursorRequest(request); err != nil {
		return Invocation{}, err
	}
	if err := a.rejectsConfigured(request.Model != "", "model", "remove the configured model or leave Request.Model empty", "model"); err != nil {
		return Invocation{}, err
	}
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
	args, stdin, err := appendPrompt(args, request.Prompt, "", false)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Cursor, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}
func validateCursorRequest(request Request) error {
	if request.Prompt.Source != PromptNone && request.Prompt.Source != PromptStdin {
		return unsupported(Cursor, NonInteractive, "prompt transport", string(request.Prompt.Source), "send the prompt over stdin")
	}
	if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText && request.OutputFormat != OutputJSON && request.OutputFormat != OutputJSONL {
		return unsupported(Cursor, NonInteractive, "output format", string(request.OutputFormat), "request text, json, or jsonl")
	}
	if request.Approval != ApprovalDefault && request.Approval != ApprovalNever && request.Approval != ApprovalBypass {
		return unsupported(Cursor, NonInteractive, "approval mode", string(request.Approval), "request never, bypass, or use the default")
	}
	return nil
}
