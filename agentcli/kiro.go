package agentcli

import "fmt"

// NewKiro returns a Kiro CLI adapter after validating configured global
// options. A zero Command uses "kiro-cli".
func NewKiro(command Command) (Adapter, error) {
	return newAdapter(Kiro, command, "kiro-cli", kiroOptionGrammar, kiroCapabilities, buildKiro)
}

var kiroOptionGrammar = optionGrammar{
	"--verbose": flag("verbose"), "-v": flag("verbose"), "--agent": value("agent"),
	"--require-mcp-startup": flag("require-mcp-startup"), "--wrap": value("wrap"),
	"--no-interactive": forbidden("mode belongs in Request.Mode"),
	"--resume":         forbidden("resume selects a session"), "-r": forbidden("resume selects a session"),
	"--resume-picker": forbidden("resume-picker selects a session"), "--resume-id": forbidden("resume-id selects a session"),
	"--list-sessions": forbidden("list-sessions is an action"), "--delete-session": forbidden("delete-session is an action"),
	"--list-models":     forbidden("list-models is an action"),
	"--trust-all-tools": flag("approval-bypass"), "--trust-tools": value("allowed-tools"),
	"--effort": value("reasoning"),
	"-h":       forbidden("help is an action, not a launch option"), "--help": forbidden("help is an action, not a launch option"),
	"-V": forbidden("version is an action, not a launch option"), "--version": forbidden("version is an action, not a launch option"),
}

var kiroCapabilities = Capabilities{
	Modes:           []Mode{NonInteractive},
	PromptSources:   []PromptSource{PromptArgument},
	Resume:          true,
	OutputFormats:   []OutputFormat{OutputText},
	ReasoningLevels: []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	ApprovalModes:   []ApprovalMode{ApprovalBypass},
	Tools:           ToolCapabilities{AllowList: true},
}

func buildKiro(a *adapter, sessionID string, request Request) (Invocation, error) {
	args := []string{a.executable, "chat"}
	args = append(args, a.options...)
	args = append(args, "--no-interactive")
	if sessionID != "" {
		args = append(args, "--resume-id", sessionID)
	}
	if request.Reasoning != ReasoningDefault {
		args = append(args, "--effort", reasoningValue(request.Reasoning))
	}
	if request.Approval == ApprovalBypass {
		args = append(args, "--trust-all-tools")
	}
	if len(request.AllowedTools) != 0 {
		args = append(args, "--trust-tools", joinComma(request.AllowedTools))
	}
	if request.Prompt.Source == PromptArgument {
		if err := validatePrompt(request.Prompt); err != nil {
			return Invocation{}, err
		}
		args = append(args, "--")
		promptArgs, _, err := appendPrompt(args, request.Prompt, "", false)
		if err != nil {
			return Invocation{}, fmt.Errorf("build %s invocation: %w", Kiro, err)
		}
		return Invocation{Argv: promptArgs}, nil
	}
	return Invocation{Argv: args}, nil
}
