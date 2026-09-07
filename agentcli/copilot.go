package agentcli

import (
	"fmt"
)

// NewCopilot returns a GitHub Copilot CLI adapter after validating configured
// options. A zero Command uses "copilot".
func NewCopilot(command Command) (Adapter, error) {
	return newAdapter(Copilot, command, "copilot", copilotOptionGrammar, copilotCapabilities, buildCopilot)
}

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

func buildCopilot(a *adapter, sessionID string, request Request) (Invocation, error) {
	// Copilot requires --allow-all-tools in noninteractive prompt mode. Callers
	// can still restrict automatic tool use with --deny-tool rules.
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
		args = append(args, "--reasoning-effort", reasoningValue(request.Reasoning))
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
