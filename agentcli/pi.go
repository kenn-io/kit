package agentcli

import (
	"fmt"
	"strings"
)

// NewPi returns a Pi adapter. Command may include configured options; an empty
// command uses "pi".
func NewPi(command []string) Adapter {
	return &piAdapter{adapter: newAdapter(Pi, command, "pi")}
}

type piAdapter struct {
	adapter
}

var piCapabilities = Capabilities{
	Modes:                  []Mode{Interactive, NonInteractive},
	Resume:                 true,
	OutputFormats:          []OutputFormat{OutputText, OutputJSONL},
	JSONSchemaInline:       true,
	JSONSchemaOutputPath:   true,
	Model:                  true,
	Provider:               true,
	Reasoning:              true,
	Tools:                  ToolCapabilities{AllowList: true, DenyList: true, DisableBuiltIns: true},
	SkillPaths:             true,
	DisableSkills:          true,
	DisableHooks:           DisableExtensionDiscovery,
	DisableExtensions:      true,
	DisablePromptTemplates: true,
	DisableThemes:          true,
	DisableContextFiles:    true,
	DisableSessionStorage:  true,
}

func (a *piAdapter) Capabilities() Capabilities {
	return cloneCapabilities(piCapabilities)
}

func (a *piAdapter) Start(request Request) (Invocation, error) {
	return a.build("", request)
}

func (a *piAdapter) Resume(sessionID string, request Request) (Invocation, error) {
	sessionID, err := validateSessionID(sessionID)
	if err != nil {
		return Invocation{}, err
	}
	return a.build(sessionID, request)
}

func (a *piAdapter) build(sessionID string, request Request) (Invocation, error) {
	mode, err := invocationMode(request.Mode)
	if err != nil {
		return Invocation{}, err
	}
	args, err := a.base()
	if err != nil {
		return Invocation{}, err
	}
	if err := validatePiRequest(mode, request); err != nil {
		return Invocation{}, err
	}
	if request.DisableSessionStorage {
		args = append(args, "--no-session")
	}
	if request.DisableHooks || request.DisableExtensions {
		args = append(args, "--no-extensions")
	}
	if request.DisableBuiltInTools {
		args = append(args, "--no-builtin-tools")
	}
	if request.DisableSkills {
		args = append(args, "--no-skills")
	}
	if request.DisablePromptTemplates {
		args = append(args, "--no-prompt-templates")
	}
	if request.DisableThemes {
		args = append(args, "--no-themes")
	}
	if request.DisableContextFiles {
		args = append(args, "--no-context-files")
	}
	for _, path := range request.SkillPaths {
		args = append(args, "--skill", path)
	}
	if request.Schema.Inline != "" {
		args = append(args,
			"--extension", request.Schema.Extension,
			"--json-schema", request.Schema.Inline,
			"--json-output", request.Schema.OutputPath,
		)
		fallback := request.Schema.Fallback
		if fallback == "" {
			fallback = "none"
		}
		args = append(args, "--json-fallback", fallback)
	}
	if mode == NonInteractive {
		args = append(args, "--print")
	}
	if request.OutputFormat == OutputJSONL {
		args = append(args, "--mode", "json")
	}
	if sessionID != "" {
		args = append(args, "--session", sessionID)
	}
	if request.Provider != "" {
		args = append(args, "--provider", request.Provider)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Reasoning != ReasoningDefault {
		args = append(args, "--thinking", piReasoning(request.Reasoning))
	}
	if len(request.AllowedTools) != 0 {
		args = append(args, "--tools", strings.Join(request.AllowedTools, ","))
	}
	if len(request.DeniedTools) != 0 {
		args = append(args, "--exclude-tools", strings.Join(request.DeniedTools, ","))
	}
	args, stdin, err := appendPrompt(args, request.Prompt, "", true)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Pi, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}

func validatePiRequest(mode Mode, request Request) error {
	if err := validateValues("allowed tools", request.AllowedTools); err != nil {
		return err
	}
	if err := validateValues("denied tools", request.DeniedTools); err != nil {
		return err
	}
	if err := validateValues("skill paths", request.SkillPaths); err != nil {
		return err
	}
	if mode == Interactive && request.Prompt.Source == PromptStdin {
		return unsupported(Pi, mode, "stdin prompt", "", "use argument delivery for an interactive prompt")
	}
	if request.Sandbox != SandboxDefault {
		return unsupported(Pi, mode, "sandbox", string(request.Sandbox), "restrict Pi through its tool allowlist or an external sandbox")
	}
	if request.Approval != ApprovalDefault {
		return unsupported(Pi, mode, "approval mode", string(request.Approval), "Pi exposes project trust, not tool approval policy")
	}
	if request.DisableUserConfig || len(request.ConfigOverrides) != 0 {
		return unsupported(Pi, mode, "Codex config controls", "", "use configured Pi options")
	}
	if request.OutputPath != "" || request.Schema.Path != "" {
		return unsupported(Pi, mode, "output path", "", "Pi output files require its JSON-schema extension")
	}
	if request.Schema.Inline != "" {
		if mode != NonInteractive {
			return unsupported(Pi, mode, "JSON schema", "", "use noninteractive mode")
		}
		if strings.TrimSpace(request.Schema.Extension) == "" || strings.TrimSpace(request.Schema.OutputPath) == "" {
			return fmt.Errorf("agent %q JSON schema requires Schema.Extension and Schema.OutputPath", Pi)
		}
		if request.Schema.Fallback != "" && request.Schema.Fallback != "none" && request.Schema.Fallback != "force" && request.Schema.Fallback != "best-effort" {
			return unsupported(Pi, mode, "JSON fallback", request.Schema.Fallback, "request none, force, or best-effort")
		}
	} else if request.Schema.Extension != "" || request.Schema.OutputPath != "" || request.Schema.Fallback != "" {
		return fmt.Errorf("agent %q schema extension options require Schema.Inline", Pi)
	}
	if mode == Interactive && request.OutputFormat != OutputDefault && request.OutputFormat != OutputText {
		return unsupported(Pi, mode, "output format", string(request.OutputFormat), "use noninteractive mode for jsonl")
	}
	if request.OutputFormat == OutputJSON {
		return unsupported(Pi, mode, "output format", string(OutputJSON), "Pi's native event output is jsonl")
	}
	if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText && request.OutputFormat != OutputJSONL {
		return unsupported(Pi, mode, "output format", string(request.OutputFormat), "request text or jsonl")
	}
	if request.Reasoning != ReasoningDefault && piReasoning(request.Reasoning) == "" {
		return unsupported(Pi, mode, "reasoning", string(request.Reasoning), "request low, medium, high, or maximum")
	}
	return nil
}

func piReasoning(level ReasoningLevel) string {
	switch level {
	case ReasoningLow, ReasoningMedium, ReasoningHigh:
		return string(level)
	case ReasoningMaximum:
		return "high"
	default:
		return ""
	}
}
