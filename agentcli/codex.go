package agentcli

import (
	"fmt"
)

// NewCodex returns a Codex CLI adapter after validating its configured global
// options. A zero Command uses "codex".
func NewCodex(command Command) (Adapter, error) {
	return newAdapter(Codex, command, "codex", codexOptionGrammar, codexCapabilities, buildCodex)
}

var codexOptionGrammar = optionGrammar{
	"-c": value("config"), "--config": value("config"),
	"--enable": value("enable"), "--disable": value("disable"),
	"--remote": value("remote"), "--remote-auth-token-env": value("remote-auth-token-env"),
	"--strict-config": flag("strict-config"),
	"-i":              value("image"), "--image": value("image"),
	"-m": value("model"), "--model": value("model"),
	"--oss": flag("oss"), "--local-provider": value("local-provider"),
	"-p": value("profile"), "--profile": value("profile"),
	"-s": value("sandbox"), "--sandbox": value("sandbox"),
	"--approve-for-me":                           flag("approve-for-me"),
	"--dangerously-bypass-approvals-and-sandbox": flag("approval-bypass"),
	"--dangerously-bypass-hook-trust":            flag("hook-trust"),
	"-C":                                         value("cd"), "--cd": value("cd"), "--add-dir": value("add-dir"),
	"-a": value("approval"), "--ask-for-approval": value("approval"),
	"--search": flag("search"), "--no-alt-screen": flag("no-alt-screen"),
	"-h":        forbidden("help is an action, not a launch option"),
	"--help":    forbidden("help is an action, not a launch option"),
	"-V":        forbidden("version is an action, not a launch option"),
	"--version": forbidden("version is an action, not a launch option"),
}

var codexCapabilities = Capabilities{
	Modes:                 []Mode{Interactive, NonInteractive},
	PromptSources:         []PromptSource{PromptArgument, PromptStdin},
	Resume:                true,
	OutputFormats:         []OutputFormat{OutputText, OutputJSONL},
	JSONSchemaPath:        true,
	JSONSchemaOutputPath:  true,
	Model:                 true,
	ReasoningLevels:       []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	SandboxModes:          []SandboxMode{SandboxReadOnly, SandboxWorkspaceWrite, SandboxDangerFullAccess},
	ApprovalModes:         []ApprovalMode{ApprovalOnRequest, ApprovalNever, ApprovalBypass},
	DisableSkills:         true,
	DisableHooks:          DisableHooksOnly,
	DisableUserConfig:     true,
	DisableSessionStorage: true,
	ConfigOverrides:       true,
}

func buildCodex(a *adapter, sessionID string, request Request) (Invocation, error) {
	mode := request.Mode
	args := a.base()
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
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", reasoningValue(request.Reasoning)))
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
	if mode == Interactive && request.Prompt.Source == PromptStdin {
		return unsupported(Codex, mode, "stdin prompt", "", "use argument delivery for an interactive prompt")
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
	if request.Approval == ApprovalBypass && request.Sandbox != SandboxDefault {
		return fmt.Errorf("agent %q cannot combine approval bypass with sandbox %q", Codex, request.Sandbox)
	}
	return nil
}
