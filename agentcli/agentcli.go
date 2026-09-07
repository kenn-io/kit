// Package agentcli builds command lines for supported coding-agent CLIs.
//
// The package does not start processes or own terminal, session-storage, or
// persistence concerns. Callers retain those responsibilities and may use the
// returned Stdin value with os/exec when the prompt is delivered over stdin.
package agentcli

import (
	"fmt"
	"slices"
	"strings"
)

// Name identifies a supported agent CLI family.
type Name string

const (
	Codex  Name = "codex"
	Claude Name = "claude"
	Pi     Name = "pi"
)

// Mode selects an interactive terminal session or a one-shot invocation.
type Mode string

const (
	Interactive    Mode = "interactive"
	NonInteractive Mode = "noninteractive"
)

// OutputFormat selects the process output contract.
type OutputFormat string

const (
	OutputDefault OutputFormat = ""
	OutputText    OutputFormat = "text"
	OutputJSON    OutputFormat = "json"
	OutputJSONL   OutputFormat = "jsonl"
)

// PromptSource describes how a prompt reaches the agent process.
type PromptSource string

const (
	PromptNone     PromptSource = ""
	PromptArgument PromptSource = "argument"
	PromptStdin    PromptSource = "stdin"
)

// Prompt is an optional initial or resumed-turn prompt. Files are supported by
// agents whose command line has a native file-reference syntax.
type Prompt struct {
	Source PromptSource
	Text   string
	Files  []string
}

// ReasoningLevel is the portable subset of agent reasoning controls.
type ReasoningLevel string

const (
	ReasoningDefault ReasoningLevel = ""
	ReasoningLow     ReasoningLevel = "low"
	ReasoningMedium  ReasoningLevel = "medium"
	ReasoningHigh    ReasoningLevel = "high"
	ReasoningMaximum ReasoningLevel = "maximum"
)

// SandboxMode selects restrictions for model-generated commands.
type SandboxMode string

const (
	SandboxDefault          SandboxMode = ""
	SandboxReadOnly         SandboxMode = "read-only"
	SandboxWorkspaceWrite   SandboxMode = "workspace-write"
	SandboxDangerFullAccess SandboxMode = "danger-full-access"
)

// ApprovalMode selects how tool approval requests are handled.
type ApprovalMode string

const (
	ApprovalDefault   ApprovalMode = ""
	ApprovalOnRequest ApprovalMode = "on-request"
	ApprovalNever     ApprovalMode = "never"
	ApprovalBypass    ApprovalMode = "bypass"
)

// JSONSchema configures a CLI's native structured-output mechanism. Codex
// accepts Path, while Claude and Pi accept Inline. Pi additionally requires an
// Extension and OutputPath.
type JSONSchema struct {
	Inline     string
	Path       string
	OutputPath string
	Extension  string
	Fallback   string
}

// Request describes one agent turn. Its zero value requests an interactive
// invocation using the agent's configured defaults. DisableExtensions,
// DisableSkills, and DisableHooks control discovery; explicit configured
// command options remain the caller's responsibility.
type Request struct {
	Mode                   Mode
	Prompt                 Prompt
	Model                  string
	Provider               string
	Reasoning              ReasoningLevel
	OutputFormat           OutputFormat
	OutputPath             string
	Schema                 JSONSchema
	Sandbox                SandboxMode
	Approval               ApprovalMode
	AllowedTools           []string
	DeniedTools            []string
	DisableBuiltInTools    bool
	SkillPaths             []string
	DisableSkills          bool
	DisableHooks           bool
	DisableExtensions      bool
	DisablePromptTemplates bool
	DisableThemes          bool
	DisableContextFiles    bool
	DisableUserConfig      bool
	DisableSessionStorage  bool
	ConfigOverrides        []string
}

// DisableScope describes what a CLI must turn off to disable hooks.
type DisableScope string

const (
	DisableUnsupported        DisableScope = "unsupported"
	DisableHooksOnly          DisableScope = "hooks-only"
	DisableAllCustomizations  DisableScope = "all-customizations"
	DisableExtensionDiscovery DisableScope = "extension-discovery"
)

// ToolCapabilities describes native tool-selection flags.
type ToolCapabilities struct {
	AllowList       bool
	DenyList        bool
	DisableBuiltIns bool
}

// Capabilities reports which Request fields an adapter can honor.
type Capabilities struct {
	Modes                  []Mode
	Resume                 bool
	OutputFormats          []OutputFormat
	JSONSchemaInline       bool
	JSONSchemaPath         bool
	JSONSchemaOutputPath   bool
	Model                  bool
	Provider               bool
	Reasoning              bool
	SandboxModes           []SandboxMode
	ApprovalModes          []ApprovalMode
	Tools                  ToolCapabilities
	SkillPaths             bool
	DisableSkills          bool
	DisableHooks           DisableScope
	DisableExtensions      bool
	DisablePromptTemplates bool
	DisableThemes          bool
	DisableContextFiles    bool
	DisableUserConfig      bool
	DisableSessionStorage  bool
	ConfigOverrides        bool
}

// Invocation is a complete argv plus optional stdin content. Argv includes the
// configured executable and its configured options.
type Invocation struct {
	Argv  []string
	Stdin *string
}

// Adapter builds start and resume invocations for one CLI family.
type Adapter interface {
	Name() Name
	Capabilities() Capabilities
	Start(Request) (Invocation, error)
	Resume(sessionID string, request Request) (Invocation, error)
}

// UnsupportedOptionError reports a requested option that an adapter cannot
// represent without changing its meaning.
type UnsupportedOptionError struct {
	Agent  Name
	Option string
	Value  string
	Mode   Mode
	Hint   string
}

func (e *UnsupportedOptionError) Error() string {
	message := fmt.Sprintf("agent %q does not support %s", e.Agent, e.Option)
	if e.Value != "" {
		message += "=" + fmt.Sprintf("%q", e.Value)
	}
	if e.Mode != "" {
		message += " in " + string(e.Mode) + " mode"
	}
	if e.Hint != "" {
		message += "; " + e.Hint
	}
	return message
}

type adapter struct {
	name    Name
	command []string
}

func newAdapter(name Name, command []string, defaultCommand string) adapter {
	if len(command) == 0 {
		command = []string{defaultCommand}
	}
	return adapter{name: name, command: slices.Clone(command)}
}

func (a adapter) Name() Name {
	return a.name
}

func (a adapter) base() ([]string, error) {
	if len(a.command) == 0 || strings.TrimSpace(a.command[0]) == "" {
		return nil, fmt.Errorf("agent %q requires a configured executable", a.name)
	}
	return slices.Clone(a.command), nil
}

func invocationMode(mode Mode) (Mode, error) {
	if mode == "" {
		return Interactive, nil
	}
	if mode != Interactive && mode != NonInteractive {
		return "", fmt.Errorf("unknown agent invocation mode %q", mode)
	}
	return mode, nil
}

func validateSessionID(sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.HasPrefix(sessionID, "-") {
		return "", fmt.Errorf("agent resume requires a session ID that does not begin with '-'")
	}
	return sessionID, nil
}

func validatePrompt(prompt Prompt) error {
	switch prompt.Source {
	case PromptNone:
		if prompt.Text != "" || len(prompt.Files) != 0 {
			return fmt.Errorf("agent prompt source is required when prompt content is set")
		}
	case PromptArgument, PromptStdin:
	default:
		return fmt.Errorf("unknown agent prompt source %q", prompt.Source)
	}
	if prompt.Source == PromptStdin && len(prompt.Files) != 0 {
		return fmt.Errorf("agent prompt files require argument delivery")
	}
	return nil
}

func validateValues(option string, values []string) error {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("agent %s contains an empty value", option)
		}
	}
	return nil
}

func unsupported(name Name, mode Mode, option, value, hint string) error {
	return &UnsupportedOptionError{Agent: name, Option: option, Value: value, Mode: mode, Hint: hint}
}

func appendPrompt(args []string, prompt Prompt, stdinMarker string, supportsFiles bool) ([]string, *string, error) {
	if err := validatePrompt(prompt); err != nil {
		return nil, nil, err
	}
	if len(prompt.Files) != 0 && !supportsFiles {
		return nil, nil, fmt.Errorf("agent does not support prompt file arguments")
	}
	switch prompt.Source {
	case PromptNone:
		return args, nil, nil
	case PromptStdin:
		if stdinMarker != "" {
			args = append(args, stdinMarker)
		}
		return args, new(prompt.Text), nil
	case PromptArgument:
		for _, file := range prompt.Files {
			if strings.TrimSpace(file) == "" {
				return nil, nil, fmt.Errorf("agent prompt file path is empty")
			}
			args = append(args, "@"+file)
		}
		if prompt.Text != "" {
			args = append(args, prompt.Text)
		}
		return args, nil, nil
	default:
		panic("prompt source validated above")
	}
}

func cloneCapabilities(capabilities Capabilities) Capabilities {
	capabilities.Modes = slices.Clone(capabilities.Modes)
	capabilities.OutputFormats = slices.Clone(capabilities.OutputFormats)
	capabilities.SandboxModes = slices.Clone(capabilities.SandboxModes)
	capabilities.ApprovalModes = slices.Clone(capabilities.ApprovalModes)
	return capabilities
}
