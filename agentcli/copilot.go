package agentcli

import (
	"fmt"
)

// NewCopilot returns a GitHub Copilot CLI adapter after validating configured
// options. A zero Command uses "copilot".
func NewCopilot(command Command) (Adapter, error) {
	base, err := newAdapter(Copilot, command, "copilot", copilotOptionGrammar)
	if err != nil {
		return nil, err
	}
	return &copilotAdapter{adapter: base}, nil
}

type copilotAdapter struct{ adapter }

var copilotOptionGrammar = optionGrammar{
	"--add-dir": value("add-dir"), "--agent": value("agent"), "--additional-mcp-config": value("mcp-config"),
	"--attachment": value("attachment"), "-C": value("cd"), "--context": value("context"),
	"--disable-builtin-mcps": flag("disable-builtin-mcps"), "--disable-mcp-server": value("disable-mcp-server"),
	"--effort": value("reasoning"), "--reasoning-effort": value("reasoning"), "--model": value("model"),
	"--allow-all": flag("approval-bypass"), "--yolo": flag("approval-bypass"), "--allow-all-tools": flag("allow-all-tools"),
	"--allow-tool": value("allowed-tools"), "--deny-tool": value("denied-tools"),
	"--output-format": value("output-format"), "--stream": value("stream"),
	"--no-auto-update": flag("no-auto-update"), "--no-color": flag("no-color"),
	"--no-custom-instructions": flag("no-custom-instructions"), "--plugin-dir": value("plugin-dir"),
	"--screen-reader": flag("screen-reader"), "--secret-env-vars": value("secret-env-vars"),
	"--acp": forbidden("ACP server mode is a different protocol"),
	"-p":    forbidden("prompt belongs in Request.Prompt"), "--prompt": forbidden("prompt belongs in Request.Prompt"),
	"-i": forbidden("interactive prompt belongs in Request.Prompt"), "--interactive": forbidden("interactive prompt belongs in Request.Prompt"),
	"--continue": forbidden("continue selects a session"), "-r": forbidden("resume selects a session"),
	"--resume": forbidden("resume selects a session"), "--session-id": forbidden("session identity belongs in Start or Resume"),
	"--connect": forbidden("remote session process ownership is a caller concern"),
	"--share":   forbidden("sharing is a caller concern"), "--share-gist": forbidden("sharing is a caller concern"),
	"-h": forbidden("help is an action, not a launch option"), "--help": forbidden("help is an action, not a launch option"),
	"-v": forbidden("version is an action, not a launch option"), "--version": forbidden("version is an action, not a launch option"),
}

var copilotCapabilities = Capabilities{
	Modes: []Mode{NonInteractive}, PromptSources: []PromptSource{PromptArgument}, Resume: true,
	OutputFormats:      []OutputFormat{OutputText, OutputJSONL},
	Model:              true,
	ReasoningLevels:    []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	ApprovalModes:      []ApprovalMode{ApprovalBypass},
	Tools:              ToolCapabilities{AllowList: true, DenyList: true},
	DisableBuiltInMCPs: true, DisableContextFiles: true,
}

func (a *copilotAdapter) Capabilities() Capabilities                { return cloneCapabilities(copilotCapabilities) }
func (a *copilotAdapter) Start(request Request) (Invocation, error) { return a.build("", request) }
func (a *copilotAdapter) Resume(sessionID string, request Request) (Invocation, error) {
	sessionID, err := validateSessionID(sessionID)
	if err != nil {
		return Invocation{}, err
	}
	return a.build(sessionID, request)
}
func (a *copilotAdapter) build(sessionID string, request Request) (Invocation, error) {
	mode, err := invocationMode(request.Mode)
	if err != nil {
		return Invocation{}, err
	}
	if mode != NonInteractive {
		return Invocation{}, unsupported(Copilot, mode, "mode", string(mode), "request noninteractive mode")
	}
	if err := validateSupportedRequest(Copilot, mode, request, copilotCapabilities); err != nil {
		return Invocation{}, err
	}
	if err := validateCopilotRequest(request); err != nil {
		return Invocation{}, err
	}
	if err := a.rejectsConfigured(request.Model != "", "model", "remove the configured model or leave Request.Model empty", "model"); err != nil {
		return Invocation{}, err
	}
	if err := a.rejectsConfigured(request.Reasoning != ReasoningDefault, "reasoning", "remove the configured reasoning level or leave Request.Reasoning empty", "reasoning"); err != nil {
		return Invocation{}, err
	}
	checks := []struct {
		requested bool
		option    string
		hint      string
		names     []string
	}{
		{request.OutputFormat != OutputDefault, "output format", "remove the configured output options or leave Request.OutputFormat empty", []string{"output-format", "stream"}},
		{request.Approval != ApprovalDefault, "approval mode", "remove the configured approval option or leave Request.Approval empty", []string{"approval-bypass"}},
		{len(request.AllowedTools) != 0, "allowed tools", "remove configured allowed tools or leave Request.AllowedTools empty", []string{"allowed-tools"}},
		{len(request.DeniedTools) != 0, "denied tools", "remove configured denied tools or leave Request.DeniedTools empty", []string{"denied-tools"}},
		{request.DisableBuiltInMCPs, "built-in MCP servers", "remove --disable-builtin-mcps or leave Request.DisableBuiltInMCPs false", []string{"disable-builtin-mcps"}},
		{request.DisableContextFiles, "custom instructions", "remove --no-custom-instructions or leave Request.DisableContextFiles false", []string{"no-custom-instructions"}},
	}
	for _, check := range checks {
		if err := a.rejectsConfigured(check.requested, check.option, check.hint, check.names...); err != nil {
			return Invocation{}, err
		}
	}
	args := append(a.base(), "--silent", "--allow-all-tools")
	if request.OutputFormat == OutputJSONL {
		args = append(args, "--stream", "off", "--output-format", "json")
	}
	if sessionID != "" {
		args = append(args, "--resume="+sessionID)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Reasoning != ReasoningDefault {
		args = append(args, "--reasoning-effort", copilotReasoning(request.Reasoning))
	}
	if request.Approval == ApprovalBypass {
		args = append(args, "--allow-all")
	}
	for _, tool := range request.AllowedTools {
		args = append(args, "--allow-tool", tool)
	}
	for _, tool := range request.DeniedTools {
		args = append(args, "--deny-tool", tool)
	}
	if request.DisableBuiltInMCPs {
		args = append(args, "--disable-builtin-mcps")
	}
	if request.DisableContextFiles {
		args = append(args, "--no-custom-instructions")
	}
	if err := validatePrompt(request.Prompt); err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Copilot, err)
	}
	if request.Prompt.Source == PromptArgument {
		args = append(args, "--prompt", request.Prompt.Text)
	}
	return Invocation{Argv: args}, nil
}
func validateCopilotRequest(request Request) error {
	if request.Prompt.Source != PromptNone && request.Prompt.Source != PromptArgument {
		return unsupported(Copilot, NonInteractive, "prompt transport", string(request.Prompt.Source), "send the prompt with --prompt")
	}
	if len(request.Prompt.Files) != 0 {
		return unsupported(Copilot, NonInteractive, "prompt files", "", "use configured --attachment options")
	}
	if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText && request.OutputFormat != OutputJSONL {
		return unsupported(Copilot, NonInteractive, "output format", string(request.OutputFormat), "request text or jsonl")
	}
	if request.Approval != ApprovalDefault && request.Approval != ApprovalBypass {
		return unsupported(Copilot, NonInteractive, "approval mode", string(request.Approval), "request bypass or use explicit tool lists")
	}
	if request.Reasoning != ReasoningDefault && copilotReasoning(request.Reasoning) == "" {
		return unsupported(Copilot, NonInteractive, "reasoning", string(request.Reasoning), "request low, medium, high, xhigh, or maximum")
	}
	if err := validateValues("allowed tools", request.AllowedTools); err != nil {
		return err
	}
	if err := validateValues("denied tools", request.DeniedTools); err != nil {
		return err
	}
	return nil
}
func copilotReasoning(level ReasoningLevel) string {
	switch level {
	case ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh:
		return string(level)
	case ReasoningMaximum:
		return "max"
	default:
		return ""
	}
}
