package agentcli

import (
	"fmt"
	"strings"
)

// NewClaude returns a Claude Code adapter. Command may include configured
// options; an empty command uses "claude".
func NewClaude(command []string) Adapter {
	return &claudeAdapter{adapter: newAdapter(Claude, command, "claude")}
}

type claudeAdapter struct {
	adapter
}

var claudeCapabilities = Capabilities{
	Modes:                 []Mode{Interactive, NonInteractive},
	Resume:                true,
	OutputFormats:         []OutputFormat{OutputText, OutputJSON, OutputJSONL},
	JSONSchemaInline:      true,
	Model:                 true,
	Reasoning:             true,
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
	args, err := a.base()
	if err != nil {
		return Invocation{}, err
	}
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
	if request.Sandbox != SandboxDefault {
		return unsupported(Claude, mode, "sandbox", string(request.Sandbox), "Claude permission modes do not provide a filesystem sandbox")
	}
	if request.DisableBuiltInTools && len(request.AllowedTools) != 0 {
		return fmt.Errorf("agent %q cannot disable built-in tools and set an allowed tool list", Claude)
	}
	if len(request.SkillPaths) != 0 {
		return unsupported(Claude, mode, "skill paths", "", "install skills through Claude configuration")
	}
	if request.DisableExtensions || request.DisablePromptTemplates || request.DisableThemes || request.DisableContextFiles {
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
		return unsupported(Claude, mode, "reasoning", string(request.Reasoning), "request low, medium, high, or maximum")
	}
	if request.Approval != ApprovalDefault && request.Approval != ApprovalOnRequest && request.Approval != ApprovalNever && request.Approval != ApprovalBypass {
		return unsupported(Claude, mode, "approval mode", string(request.Approval), "request on-request, never, or bypass")
	}
	return nil
}

func claudeReasoning(level ReasoningLevel) string {
	switch level {
	case ReasoningLow, ReasoningMedium, ReasoningHigh:
		return string(level)
	case ReasoningMaximum:
		return "max"
	default:
		return ""
	}
}
