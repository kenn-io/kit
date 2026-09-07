package agentcli

import (
	"fmt"
	"strings"
)

// NewPi returns a Pi adapter after validating its configured options. A zero
// Command uses "pi".
func NewPi(command Command) (Adapter, error) {
	base, err := newAdapter(Pi, command, "pi", piOptionGrammar)
	if err != nil {
		return nil, err
	}
	return &piAdapter{adapter: base}, nil
}

type piAdapter struct {
	adapter
}

var piOptionGrammar = optionGrammar{
	"--provider": value("provider"), "--model": value("model"), "--api-key": value("api-key"),
	"--system-prompt": value("system-prompt"), "--append-system-prompt": value("append-system-prompt"),
	"--mode": value("mode"), "--session-dir": value("session-dir"), "--no-session": flag("no-session"),
	"--name": value("name"), "-n": value("name"), "--models": value("models"),
	"--no-tools": flag("no-tools"), "-nt": flag("no-tools"),
	"--no-builtin-tools": flag("no-builtin-tools"), "-nbt": flag("no-builtin-tools"),
	"--tools": value("tools"), "-t": value("tools"), "--exclude-tools": value("exclude-tools"), "-xt": value("exclude-tools"),
	"--thinking": value("thinking"), "--extension": value("extension"), "-e": value("extension"),
	"--no-extensions": flag("no-extensions"), "-ne": flag("no-extensions"),
	"--skill": value("skill"), "--no-skills": flag("no-skills"), "-ns": flag("no-skills"),
	"--prompt-template": value("prompt-template"), "--no-prompt-templates": flag("no-prompt-templates"), "-np": flag("no-prompt-templates"),
	"--theme": value("theme"), "--use-theme": value("use-theme"), "--no-themes": flag("no-themes"),
	"--no-context-files": flag("no-context-files"), "-nc": flag("no-context-files"),
	"--verbose": flag("verbose"), "--tui-mode": value("tui-mode"), "--approve": flag("approve"), "-a": flag("approve"),
	"--no-approve": flag("no-approve"), "-na": flag("no-approve"), "--offline": flag("offline"),
	"--mcp-config": value("mcp-config"), "--json-schema": value("json-schema"),
	"--json-output": value("json-output"), "--json-fallback": value("json-fallback"),
	"--fff-mode": value("fff-mode"), "--fff-frecency-db": value("fff-frecency-db"),
	"--fff-history-db": value("fff-history-db"), "--fff-enable-root-scan": flag("fff-enable-root-scan"),
	"--fff-enable-home-scan": flag("fff-enable-home-scan"),
	"--print":                forbidden("print mode is selected by Request.Mode"), "-p": forbidden("print mode is selected by Request.Mode"),
	"--continue": forbidden("continue selects a session"), "-c": forbidden("continue selects a session"),
	"--resume": forbidden("resume selects a session"), "-r": forbidden("resume selects a session"),
	"--session": forbidden("session selector is owned by Resume"), "--session-id": forbidden("session identity is owned by Start or Resume"),
	"--fork": forbidden("fork changes session identity"), "--export": forbidden("export is an action, not a launch option"),
	"--list-models": forbidden("list-models has an optional value and is an action"),
	"--help":        forbidden("help is an action, not a launch option"), "-h": forbidden("help is an action, not a launch option"),
	"--version": forbidden("version is an action, not a launch option"), "-v": forbidden("version is an action, not a launch option"),
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
	args := a.base()
	if err := validatePiRequest(mode, request); err != nil {
		return Invocation{}, err
	}
	if err := a.validateConfiguredRequest(request); err != nil {
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

func (a *piAdapter) validateConfiguredRequest(request Request) error {
	checks := []struct {
		requested bool
		option    string
		hint      string
		names     []string
	}{
		{request.Provider != "", "provider", "remove the configured provider or leave Request.Provider empty", []string{"provider"}},
		{request.Model != "", "model", "remove the configured model or leave Request.Model empty", []string{"model"}},
		{request.Reasoning != ReasoningDefault, "reasoning", "remove the configured thinking level or leave Request.Reasoning empty", []string{"thinking"}},
		{request.OutputFormat != OutputDefault, "output format", "remove the configured mode or leave Request.OutputFormat empty", []string{"mode"}},
		{request.Schema.Inline != "", "JSON schema", "remove configured schema options or leave Request.Schema empty", []string{"json-schema", "json-output", "json-fallback"}},
		{request.DisableBuiltInTools || len(request.AllowedTools) != 0, "allowed tools", "remove configured tool selection or leave request tool selection empty", []string{"no-tools", "no-builtin-tools", "tools"}},
		{len(request.DeniedTools) != 0, "denied tools", "remove configured excluded tools or leave Request.DeniedTools empty", []string{"exclude-tools"}},
		{len(request.SkillPaths) != 0 || request.DisableSkills, "skills", "remove configured skill options or leave request skill controls empty", []string{"skill", "no-skills"}},
		{request.DisableHooks || request.DisableExtensions, "extensions", "remove configured extension controls or leave request disable controls false", []string{"extension", "no-extensions"}},
		{request.DisablePromptTemplates, "prompt templates", "remove configured prompt-template controls or leave Request.DisablePromptTemplates false", []string{"prompt-template", "no-prompt-templates"}},
		{request.DisableThemes, "themes", "remove configured theme controls or leave Request.DisableThemes false", []string{"theme", "use-theme", "no-themes"}},
		{request.DisableContextFiles, "context files", "remove --no-context-files or leave Request.DisableContextFiles false", []string{"no-context-files"}},
		{request.DisableSessionStorage, "session persistence", "remove --no-session or leave Request.DisableSessionStorage false", []string{"no-session"}},
	}
	for _, check := range checks {
		if err := a.rejectsConfigured(check.requested, check.option, check.hint, check.names...); err != nil {
			return err
		}
	}
	return nil
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
