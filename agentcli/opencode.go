package agentcli

import "fmt"

// NewOpenCode returns an OpenCode adapter after validating configured global
// options. A zero Command uses "opencode".
func NewOpenCode(command Command) (Adapter, error) {
	return newAdapter(OpenCode, command, "opencode", openCodeOptionGrammar, openCodeCapabilities, buildOpenCode)
}

var openCodeOptionGrammar = optionGrammar{
	"--print-logs": flag("print-logs"), "--log-level": value("log-level"),
	"--pure": flag("pure"), "-m": value("model"), "--model": value("model"),
	"--agent": value("agent"),
	"-c":      forbidden("continue selects a session"), "--continue": forbidden("continue selects a session"),
	"-s": forbidden("session selects a session"), "--session": forbidden("session selects a session"),
	"--fork": forbidden("fork changes session identity"), "--prompt": forbidden("prompt belongs in Request.Prompt"),
	"--auto": forbidden("automatic permission approval belongs in Request.Approval"),
	"-h":     forbidden("help is an action, not a launch option"), "--help": forbidden("help is an action, not a launch option"),
	"-v": forbidden("version is an action, not a launch option"), "--version": forbidden("version is an action, not a launch option"),
}

var openCodeCapabilities = Capabilities{
	Modes:         []Mode{NonInteractive},
	PromptSources: []PromptSource{PromptStdin},
	Resume:        true,
	OutputFormats: []OutputFormat{OutputText, OutputJSONL},
	Model:         true,
}

func buildOpenCode(a *adapter, sessionID string, request Request) (Invocation, error) {
	args := append(a.base(), "run")
	if request.OutputFormat == OutputJSONL {
		args = append(args, "--format", "json")
	}
	if sessionID != "" {
		args = append(args, "--session", sessionID)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	args, stdin, err := appendPrompt(args, request.Prompt, "", false)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", OpenCode, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}
