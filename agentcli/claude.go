package agentcli

import (
	"fmt"
	"strings"
)

// NewClaude returns a Claude Code adapter after validating its configured
// options. A zero Command uses "claude".
func NewClaude(command Command) (Adapter, error) {
	return newAdapter(Claude, command, "claude", claudeOptionGrammar, claudeCapabilities, buildClaude)
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

func buildClaude(a *adapter, sessionID string, request Request) (Invocation, error) {
	mode := request.Mode
	args := a.base()
	if err := validateClaudeRequest(mode, request); err != nil {
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
		args = append(args, "--effort", reasoningValue(request.Reasoning))
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
	var stdin *string
	var err error
	if mode == Interactive {
		args, err = appendArgumentPrompt(args, request.Prompt, false)
	} else {
		stdin, err = stdinPrompt(request.Prompt)
	}
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Claude, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}

func validateClaudeRequest(mode Mode, request Request) error {
	if request.DisableBuiltInTools && len(request.AllowedTools) != 0 {
		return fmt.Errorf("agent %q cannot disable built-in tools and set an allowed tool list", Claude)
	}
	if mode == Interactive {
		if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText {
			return unsupported(Claude, mode, "output format", string(request.OutputFormat), "use noninteractive mode")
		}
		if request.Schema.Inline != "" || request.DisableSessionStorage {
			return unsupported(Claude, mode, "automation-only output controls", "", "use noninteractive mode")
		}
	}
	return nil
}
