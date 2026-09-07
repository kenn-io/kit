package agentcli

import (
	"fmt"
	"strings"
)

// NewClaude returns a Claude Code adapter after validating its configured
// options. A zero Command uses "claude".
func NewClaude(command Command) (Adapter, error) {
	base, err := newAdapter(Claude, command, "claude", claudeOptionGrammar)
	if err != nil {
		return nil, err
	}
	return &claudeAdapter{adapter: base}, nil
}

type claudeAdapter struct {
	adapter
}

var claudeOptionGrammar = optionGrammar{
	"--add-dir": value("add-dir"), "--agent": value("agent"), "--agents": value("agents"),
	"--allow-dangerously-skip-permissions": flag("allow-permission-bypass"),
	"--allowedTools":                       value("allowed-tools"), "--allowed-tools": value("allowed-tools"),
	"--append-system-prompt": value("append-system-prompt"), "--autocompact": value("autocompact"),
	"--ax-screen-reader": flag("screen-reader"), "--bare": flag("bare"), "--betas": value("betas"),
	"--brief": flag("brief"), "--chrome": flag("chrome"), "--dangerously-skip-permissions": flag("approval-bypass"),
	"--debug-file": value("debug-file"), "--disable-slash-commands": flag("disable-skills"),
	"--disallowedTools": value("denied-tools"), "--disallowed-tools": value("denied-tools"),
	"--effort": value("effort"), "--exclude-dynamic-system-prompt-sections": flag("exclude-dynamic-prompt"),
	"--fallback-model": value("fallback-model"), "--file": value("file"),
	"--forward-subagent-text": flag("forward-subagent-text"), "--ide": flag("ide"),
	"--include-hook-events": flag("include-hook-events"), "--include-partial-messages": flag("include-partial-messages"),
	"--input-format": value("input-format"), "--json-schema": value("json-schema"),
	"--max-budget-usd": value("max-budget-usd"), "--mcp-config": value("mcp-config"),
	"--model": value("model"), "-n": value("name"), "--name": value("name"),
	"--no-chrome": flag("no-chrome"), "--no-session-persistence": flag("no-session-persistence"),
	"--output-format": value("output-format"), "--permission-mode": value("permission-mode"),
	"--permission-prompts": value("permission-prompts"), "--plugin-dir": value("plugin-dir"),
	"--plugin-url": value("plugin-url"), "--replay-user-messages": flag("replay-user-messages"),
	"--restricted": flag("restricted"), "--safe-mode": flag("safe-mode"),
	"--setting-sources": value("setting-sources"), "--settings": value("settings"),
	"--strict-mcp-config": flag("strict-mcp-config"), "--system-prompt": value("system-prompt"),
	"--system-prompt-snapshot": value("system-prompt-snapshot"), "--tools": value("tools"),
	"--verbose": flag("verbose"),
	"-p":        forbidden("print mode is selected by Request.Mode"), "--print": forbidden("print mode is selected by Request.Mode"),
	"-c": forbidden("continue selects a session"), "--continue": forbidden("continue selects a session"),
	"-r": forbidden("resume selects a session"), "--resume": forbidden("resume selects a session"),
	"--session-id":   forbidden("session ID is owned by Start or Resume"),
	"--fork-session": forbidden("fork changes resume identity"), "--from-pr": forbidden("from-pr selects a session"),
	"--teleport": forbidden("teleport selects a session"), "--cloud": forbidden("cloud changes the command target"),
	"--environment": forbidden("environment starts a cloud session"), "--bg": forbidden("background process ownership is a caller concern"),
	"--background":                         forbidden("background process ownership is a caller concern"),
	"--remote-control":                     forbidden("remote control changes process ownership"),
	"--remote-control-session-name-prefix": forbidden("remote control changes process ownership"),
	"--tmux":                               forbidden("tmux process ownership is a caller concern"), "-w": forbidden("worktree creation is a caller concern"),
	"--worktree":           forbidden("worktree creation is a caller concern"),
	"-d":                   forbidden("debug has an optional value and is ambiguous in configured options"),
	"--debug":              forbidden("debug has an optional value and is ambiguous in configured options"),
	"--prompt-suggestions": forbidden("prompt-suggestions has an optional value and is ambiguous in configured options"),
	"-h":                   forbidden("help is an action, not a launch option"), "--help": forbidden("help is an action, not a launch option"),
	"-v": forbidden("version is an action, not a launch option"), "--version": forbidden("version is an action, not a launch option"),
}

var claudeCapabilities = Capabilities{
	Modes:                 []Mode{Interactive, NonInteractive},
	PromptSources:         []PromptSource{PromptArgument, PromptStdin},
	Resume:                true,
	OutputFormats:         []OutputFormat{OutputText, OutputJSON, OutputJSONL},
	JSONSchemaInline:      true,
	Model:                 true,
	ReasoningLevels:       []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	ApprovalModes:         []ApprovalMode{ApprovalOnRequest, ApprovalNever, ApprovalBypass},
	Tools:                 ToolCapabilities{AllowList: true, DenyList: true, DisableBuiltIns: true},
	DisableSkills:         true,
	DisableHooks:          DisableAllCustomizations,
	DisableSessionStorage: true,
}

func (a *claudeAdapter) Capabilities() Capabilities {
	return cloneCapabilities(claudeCapabilities)
}

func (a *claudeAdapter) Start(request Request) (Invocation, error) {
	return a.build("", request)
}

func (a *claudeAdapter) Resume(sessionID string, request Request) (Invocation, error) {
	sessionID, err := validateSessionID(sessionID)
	if err != nil {
		return Invocation{}, err
	}
	return a.build(sessionID, request)
}

func (a *claudeAdapter) build(sessionID string, request Request) (Invocation, error) {
	mode, err := invocationMode(request.Mode)
	if err != nil {
		return Invocation{}, err
	}
	args := a.base()
	if err := validateSupportedRequest(Claude, mode, request, claudeCapabilities); err != nil {
		return Invocation{}, err
	}
	if err := validateClaudeRequest(mode, request); err != nil {
		return Invocation{}, err
	}
	if err := a.validateConfiguredRequest(request); err != nil {
		return Invocation{}, err
	}
	if mode == NonInteractive {
		args = append(args, "--print")
	}
	if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText {
		format := string(request.OutputFormat)
		if request.OutputFormat == OutputJSONL {
			format = "stream-json"
			args = append(args, "--verbose")
		}
		args = append(args, "--output-format", format)
	}
	if request.Schema.Inline != "" {
		args = append(args, "--json-schema", request.Schema.Inline)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Reasoning != ReasoningDefault {
		args = append(args, "--effort", claudeReasoning(request.Reasoning))
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if request.DisableHooks {
		args = append(args, "--safe-mode")
	} else if request.DisableSkills {
		args = append(args, "--disable-slash-commands")
	}
	if request.DisableSessionStorage {
		args = append(args, "--no-session-persistence")
	}
	if request.Approval == ApprovalBypass {
		args = append(args, "--dangerously-skip-permissions")
	} else if request.Approval != ApprovalDefault {
		permissionMode := "manual"
		if request.Approval == ApprovalNever {
			permissionMode = "dontAsk"
		}
		args = append(args, "--permission-mode", permissionMode)
	}
	if request.DisableBuiltInTools {
		args = append(args, "--tools", "")
	} else if len(request.AllowedTools) != 0 {
		args = append(args, "--allowedTools", strings.Join(request.AllowedTools, ","))
	}
	if len(request.DeniedTools) != 0 {
		args = append(args, "--disallowedTools", strings.Join(request.DeniedTools, ","))
	}
	args, stdin, err := appendPrompt(args, request.Prompt, "", false)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Claude, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}

func (a *claudeAdapter) validateConfiguredRequest(request Request) error {
	checks := []struct {
		requested bool
		option    string
		hint      string
		names     []string
	}{
		{request.Model != "", "model", "remove the configured model or leave Request.Model empty", []string{"model"}},
		{request.Reasoning != ReasoningDefault, "reasoning", "remove the configured effort or leave Request.Reasoning empty", []string{"effort"}},
		{request.OutputFormat != OutputDefault, "output format", "remove the configured output format or leave Request.OutputFormat empty", []string{"output-format"}},
		{request.Schema.Inline != "", "JSON schema", "remove the configured schema or leave Request.Schema empty", []string{"json-schema"}},
		{request.Approval != ApprovalDefault, "approval mode", "remove the configured permission option or leave Request.Approval empty", []string{"permission-mode", "approval-bypass"}},
		{len(request.AllowedTools) != 0 || request.DisableBuiltInTools, "allowed tools", "remove configured tool selection or leave request tool selection empty", []string{"allowed-tools", "tools"}},
		{len(request.DeniedTools) != 0, "denied tools", "remove configured denied tools or leave Request.DeniedTools empty", []string{"denied-tools"}},
		{request.DisableSkills || request.DisableHooks, "customization controls", "remove configured customization controls or leave request disable controls false", []string{"disable-skills", "safe-mode", "bare"}},
		{request.DisableSessionStorage, "session persistence", "remove --no-session-persistence or leave Request.DisableSessionStorage false", []string{"no-session-persistence"}},
	}
	for _, check := range checks {
		if err := a.rejectsConfigured(check.requested, check.option, check.hint, check.names...); err != nil {
			return err
		}
	}
	return nil
}

func validateClaudeRequest(mode Mode, request Request) error {
	if err := validateValues("allowed tools", request.AllowedTools); err != nil {
		return err
	}
	if err := validateValues("denied tools", request.DeniedTools); err != nil {
		return err
	}
	if mode == Interactive && request.Prompt.Source == PromptStdin {
		return unsupported(Claude, mode, "stdin prompt", "", "use argument delivery for an interactive prompt")
	}
	if request.Provider != "" {
		return unsupported(Claude, mode, "provider", request.Provider, "configure the provider outside Claude's argv")
	}
	if request.Autonomy != AutonomyDefault {
		return unsupported(Claude, mode, "autonomy", string(request.Autonomy), "use approval and tool controls")
	}
	if request.Sandbox != SandboxDefault {
		return unsupported(Claude, mode, "sandbox", string(request.Sandbox), "Claude permission modes do not provide a filesystem sandbox")
	}
	if request.DisableBuiltInTools && len(request.AllowedTools) != 0 {
		return fmt.Errorf("agent %q cannot disable built-in tools and set an allowed tool list", Claude)
	}
	if len(request.SkillPaths) != 0 {
		return unsupported(Claude, mode, "skill paths", "", "install skills through Claude configuration")
	}
	if request.DisableExtensions || request.DisablePromptTemplates || request.DisableThemes || request.DisableContextFiles || request.DisableBuiltInMCPs {
		return unsupported(Claude, mode, "Pi customization controls", "", "these controls are specific to Pi")
	}
	if request.DisableUserConfig || len(request.ConfigOverrides) != 0 {
		return unsupported(Claude, mode, "Codex config controls", "", "use configured Claude options such as --settings")
	}
	if request.Schema.Path != "" || request.Schema.OutputPath != "" || request.Schema.Extension != "" || request.Schema.Fallback != "" || request.OutputPath != "" {
		return unsupported(Claude, mode, "schema file or output path", "", "Claude accepts an inline schema and writes structured output to stdout")
	}
	if mode == Interactive {
		if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText {
			return unsupported(Claude, mode, "output format", string(request.OutputFormat), "use noninteractive mode")
		}
		if request.Schema.Inline != "" || request.DisableSessionStorage {
			return unsupported(Claude, mode, "automation-only output controls", "", "use noninteractive mode")
		}
	}
	if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText && request.OutputFormat != OutputJSON && request.OutputFormat != OutputJSONL {
		return unsupported(Claude, mode, "output format", string(request.OutputFormat), "request text, json, or jsonl")
	}
	if request.Reasoning != ReasoningDefault && claudeReasoning(request.Reasoning) == "" {
		return unsupported(Claude, mode, "reasoning", string(request.Reasoning), "request low, medium, high, xhigh, or maximum")
	}
	if request.Approval != ApprovalDefault && request.Approval != ApprovalOnRequest && request.Approval != ApprovalNever && request.Approval != ApprovalBypass {
		return unsupported(Claude, mode, "approval mode", string(request.Approval), "request on-request, never, or bypass")
	}
	return nil
}

func claudeReasoning(level ReasoningLevel) string {
	switch level {
	case ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh:
		return string(level)
	case ReasoningMaximum:
		return "max"
	default:
		return ""
	}
}
