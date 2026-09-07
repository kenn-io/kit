package agentcli

import "fmt"

// NewDroid returns a Factory Droid CLI adapter after validating configured
// global options. A zero Command uses "droid".
func NewDroid(command Command) (Adapter, error) {
	return newAdapter(Droid, command, "droid", droidOptionGrammar, droidCapabilities, buildDroid)
}

var droidOptionGrammar = optionGrammar{
	"--disable-builtin-skills": flag("disable-builtin-skills"), "--append-system-prompt": value("append-system-prompt"),
	"--append-system-prompt-file": value("append-system-prompt-file"),
	"-m":                          value("model"), "--model": value("model"),
	"--auto": value("autonomy"), "--reasoning-effort": value("reasoning"),
	"--restrict-tools": value("allowed-tools"), "--disabled-tools": value("denied-tools"),
	"-o": value("output-format"), "--output-format": value("output-format"),
	"--skip-permissions-unsafe": flag("approval-bypass"),
	"-w":                        forbidden("worktree creation is a caller concern"), "--worktree": forbidden("worktree creation is a caller concern"),
	"--resume": forbidden("resume selects a session"), "-r": forbidden("resume has command-dependent meaning and is ambiguous in configured options"),
	"-s": forbidden("session selects a session"), "--session-id": forbidden("session selects a session"),
	"--fork": forbidden("fork changes session identity"),
	"-h":     forbidden("help is an action, not a launch option"), "--help": forbidden("help is an action, not a launch option"),
	"-v": forbidden("version is an action, not a launch option"), "--version": forbidden("version is an action, not a launch option"),
}

var droidCapabilities = Capabilities{
	Modes: []Mode{NonInteractive}, PromptSources: []PromptSource{PromptArgument, PromptStdin}, Resume: true,
	OutputFormats: []OutputFormat{OutputText, OutputJSON, OutputJSONL}, Model: true,
	ReasoningLevels: []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	AutonomyLevels:  []AutonomyLevel{AutonomyLow, AutonomyMedium, AutonomyHigh},
	ApprovalModes:   []ApprovalMode{ApprovalBypass}, Tools: ToolCapabilities{AllowList: true, DenyList: true},
	DisableSkills: true,
}

func buildDroid(a *adapter, sessionID string, request Request) (Invocation, error) {
	args := []string{a.executable, "exec"}
	args = append(args, a.options...)
	if sessionID != "" {
		args = append(args, "--session-id", sessionID)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Reasoning != ReasoningDefault {
		args = append(args, "--reasoning-effort", reasoningValue(request.Reasoning))
	}
	if request.Autonomy != AutonomyDefault {
		args = append(args, "--auto", string(request.Autonomy))
	}
	if request.Approval == ApprovalBypass {
		args = append(args, "--skip-permissions-unsafe")
	}
	if len(request.AllowedTools) != 0 {
		args = append(args, "--restrict-tools", joinComma(request.AllowedTools))
	}
	if len(request.DeniedTools) != 0 {
		args = append(args, "--disabled-tools", joinComma(request.DeniedTools))
	}
	if request.DisableSkills {
		args = append(args, "--disable-builtin-skills")
	}
	switch request.OutputFormat {
	case OutputJSON:
		args = append(args, "--output-format", "json")
	case OutputJSONL:
		args = append(args, "--output-format", "stream-json")
	}
	args, stdin, err := appendPrompt(args, request.Prompt, "", false)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Droid, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}
