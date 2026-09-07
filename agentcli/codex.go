package agentcli

import (
	"fmt"
)

// NewCodex returns a Codex CLI adapter. Command may include configured global
// options; an empty command uses "codex".
func NewCodex(command []string) Adapter {
	return &codexAdapter{adapter: newAdapter(Codex, command, "codex")}
}

type codexAdapter struct {
	adapter
}

var codexCapabilities = Capabilities{
	Modes:                 []Mode{Interactive, NonInteractive},
	Resume:                true,
	OutputFormats:         []OutputFormat{OutputText, OutputJSONL},
	JSONSchemaPath:        true,
	JSONSchemaOutputPath:  true,
	Model:                 true,
	Reasoning:             true,
	SandboxModes:          []SandboxMode{SandboxReadOnly, SandboxWorkspaceWrite, SandboxDangerFullAccess},
	ApprovalModes:         []ApprovalMode{ApprovalOnRequest, ApprovalNever, ApprovalBypass},
	DisableSkills:         true,
	DisableHooks:          DisableHooksOnly,
	DisableUserConfig:     true,
	DisableSessionStorage: true,
	ConfigOverrides:       true,
}

func (a *codexAdapter) Capabilities() Capabilities {
	return cloneCapabilities(codexCapabilities)
}

func (a *codexAdapter) Start(request Request) (Invocation, error) {
	return a.build("", request)
}

func (a *codexAdapter) Resume(sessionID string, request Request) (Invocation, error) {
	sessionID, err := validateSessionID(sessionID)
	if err != nil {
		return Invocation{}, err
	}
	return a.build(sessionID, request)
}

func (a *codexAdapter) build(sessionID string, request Request) (Invocation, error) {
	mode, err := invocationMode(request.Mode)
	if err != nil {
		return Invocation{}, err
	}
	args, err := a.base()
	if err != nil {
		return Invocation{}, err
	}
	if err := validateCodexRequest(mode, request); err != nil {
		return Invocation{}, err
	}

	if mode == NonInteractive {
		args = append(args, "exec")
	}
	if sessionID != "" {
		args = append(args, "resume")
	}

	for _, override := range request.ConfigOverrides {
		args = append(args, "-c", override)
	}
	if request.DisableUserConfig {
		args = append(args, "--ignore-user-config")
	}
	if request.DisableSkills {
		args = append(args, "-c", "skills.include_instructions=false")
	}
	if request.DisableHooks {
		args = append(args, "--disable", "hooks")
	}
	if request.DisableSessionStorage {
		args = append(args, "--ephemeral")
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Reasoning != ReasoningDefault {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", codexReasoning(request.Reasoning)))
	}
	if request.Approval == ApprovalBypass {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	} else {
		if request.Sandbox != SandboxDefault {
			if mode == NonInteractive && sessionID != "" {
				args = append(args, "-c", fmt.Sprintf("sandbox_mode=%q", request.Sandbox))
			} else {
				args = append(args, "--sandbox", string(request.Sandbox))
			}
		}
		if request.Approval != ApprovalDefault {
			if mode == NonInteractive && sessionID != "" {
				args = append(args, "-c", fmt.Sprintf("approval_policy=%q", request.Approval))
			} else {
				args = append(args, "--ask-for-approval", string(request.Approval))
			}
		}
	}
	if request.OutputFormat == OutputJSONL {
		args = append(args, "--json")
	}
	if request.Schema.Path != "" {
		args = append(args, "--output-schema", request.Schema.Path)
	}
	outputPath := request.OutputPath
	if request.Schema.OutputPath != "" {
		outputPath = request.Schema.OutputPath
	}
	if outputPath != "" {
		args = append(args, "--output-last-message", outputPath)
	}
	if sessionID != "" {
		args = append(args, sessionID)
	}
	args, stdin, err := appendPrompt(args, request.Prompt, "-", false)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Codex, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}

func validateCodexRequest(mode Mode, request Request) error {
	if err := validateValues("config overrides", request.ConfigOverrides); err != nil {
		return err
	}
	if mode == Interactive && request.Prompt.Source == PromptStdin {
		return unsupported(Codex, mode, "stdin prompt", "", "use argument delivery for an interactive prompt")
	}
	if request.Provider != "" {
		return unsupported(Codex, mode, "provider", request.Provider, "put the provider in configured Codex options")
	}
	if len(request.AllowedTools) != 0 || len(request.DeniedTools) != 0 || request.DisableBuiltInTools {
		return unsupported(Codex, mode, "tool policy", "", "Codex has no equivalent per-invocation tool-list flags")
	}
	if len(request.SkillPaths) != 0 {
		return unsupported(Codex, mode, "skill paths", "", "install skills through Codex configuration")
	}
	if request.DisableExtensions || request.DisablePromptTemplates || request.DisableThemes || request.DisableContextFiles {
		return unsupported(Codex, mode, "Pi customization controls", "", "these controls are specific to Pi")
	}
	if request.Schema.Inline != "" || request.Schema.Extension != "" || request.Schema.Fallback != "" {
		return unsupported(Codex, mode, "inline JSON schema", "", "write the schema to a file and set Schema.Path")
	}
	if request.OutputPath != "" && request.Schema.OutputPath != "" && request.OutputPath != request.Schema.OutputPath {
		return fmt.Errorf("agent %q received conflicting output paths", Codex)
	}
	if mode == Interactive {
		if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText {
			return unsupported(Codex, mode, "output format", string(request.OutputFormat), "use noninteractive mode for machine-readable output")
		}
		if request.Schema.Path != "" || request.OutputPath != "" || request.Schema.OutputPath != "" {
			return unsupported(Codex, mode, "structured output", "", "use noninteractive mode")
		}
		if request.DisableUserConfig || request.DisableSessionStorage {
			return unsupported(Codex, mode, "automation-only config controls", "", "use noninteractive mode")
		}
	}
	if request.OutputFormat == OutputJSON {
		return unsupported(Codex, mode, "output format", string(OutputJSON), "Codex emits an event stream; request jsonl")
	}
	if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText && request.OutputFormat != OutputJSONL {
		return unsupported(Codex, mode, "output format", string(request.OutputFormat), "request text or jsonl")
	}
	if request.Reasoning != ReasoningDefault && codexReasoning(request.Reasoning) == "" {
		return unsupported(Codex, mode, "reasoning", string(request.Reasoning), "request low, medium, high, or maximum")
	}
	if request.Approval == ApprovalBypass && request.Sandbox != SandboxDefault {
		return fmt.Errorf("agent %q cannot combine approval bypass with sandbox %q", Codex, request.Sandbox)
	}
	if request.Approval != ApprovalDefault && request.Approval != ApprovalOnRequest && request.Approval != ApprovalNever && request.Approval != ApprovalBypass {
		return unsupported(Codex, mode, "approval mode", string(request.Approval), "request on-request, never, or bypass")
	}
	if request.Sandbox != SandboxDefault && request.Sandbox != SandboxReadOnly && request.Sandbox != SandboxWorkspaceWrite && request.Sandbox != SandboxDangerFullAccess {
		return unsupported(Codex, mode, "sandbox", string(request.Sandbox), "request read-only, workspace-write, or danger-full-access")
	}
	return nil
}

func codexReasoning(level ReasoningLevel) string {
	switch level {
	case ReasoningLow, ReasoningMedium, ReasoningHigh:
		return string(level)
	case ReasoningMaximum:
		return "xhigh"
	default:
		return ""
	}
}
