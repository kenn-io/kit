package agentcli

import "fmt"

// NewGemini returns a Gemini CLI adapter after validating configured options.
// A zero Command uses "gemini".
func NewGemini(command Command) (Adapter, error) {
	return newAdapter(Gemini, command, "gemini", geminiOptionGrammar, geminiCapabilities, buildGemini)
}

var geminiOptionGrammar = optionGrammar{
	"-d": flag("debug"), "--debug": flag("debug"), "-m": value("model"), "--model": value("model"),
	"--skip-trust": flag("skip-trust"), "--policy": value("policy"), "--admin-policy": value("admin-policy"),
	"--allowed-mcp-server-names": value("allowed-mcp-server-names"), "--allowed-tools": value("allowed-tools"),
	"-e": value("extensions"), "--extensions": value("extensions"), "--include-directories": value("include-directories"),
	"--screen-reader": flag("screen-reader"), "--raw-output": flag("raw-output"), "--accept-raw-output-risk": flag("accept-raw-output-risk"),
	"-p": forbidden("prompt belongs in Request.Prompt"), "--prompt": forbidden("prompt belongs in Request.Prompt"),
	"-i": forbidden("interactive prompt belongs in Request.Prompt"), "--prompt-interactive": forbidden("interactive prompt belongs in Request.Prompt"),
	"-r": forbidden("resume selects a session"), "--resume": forbidden("resume selects a session"),
	"--session-file": forbidden("session file selects a session"), "--session-id": forbidden("session identity belongs in Start or Resume"),
	"--list-sessions": forbidden("list-sessions is an action"), "--delete-session": forbidden("delete-session is an action"),
	"--acp": forbidden("ACP server mode is a different protocol"), "--experimental-acp": forbidden("ACP server mode is a different protocol"),
	"-w": forbidden("worktree creation is a caller concern"), "--worktree": forbidden("worktree creation is a caller concern"),
	"-s": forbidden("sandbox selection belongs in Request.Sandbox"), "--sandbox": forbidden("sandbox selection belongs in Request.Sandbox"),
	"-y": flag("approval-bypass"), "--yolo": flag("approval-bypass"),
	"--approval-mode": value("approval-mode"),
	"-o":              value("output-format"), "--output-format": value("output-format"),
	"-l": forbidden("list-extensions is an action"), "--list-extensions": forbidden("list-extensions is an action"),
	"-h": forbidden("help is an action, not a launch option"), "--help": forbidden("help is an action, not a launch option"),
	"-v": forbidden("version is an action, not a launch option"), "--version": forbidden("version is an action, not a launch option"),
}

var geminiCapabilities = Capabilities{
	Modes: []Mode{NonInteractive}, PromptSources: []PromptSource{PromptArgument, PromptStdin}, Resume: true,
	OutputFormats: []OutputFormat{OutputText, OutputJSON, OutputJSONL}, Model: true,
	ApprovalModes: []ApprovalMode{ApprovalNever, ApprovalBypass},
}

func buildGemini(a *adapter, sessionID string, request Request) (Invocation, error) {
	args := a.base()
	if request.OutputFormat == OutputJSON {
		args = append(args, "--output-format", "json")
	}
	if request.OutputFormat == OutputJSONL {
		args = append(args, "--output-format", "stream-json")
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	switch request.Approval {
	case ApprovalNever:
		args = append(args, "--approval-mode", "plan")
	case ApprovalBypass:
		args = append(args, "--approval-mode", "yolo")
	}
	if err := validatePrompt(request.Prompt); err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Gemini, err)
	}
	var stdin *string
	switch request.Prompt.Source {
	case PromptArgument:
		args = append(args, "--prompt", request.Prompt.Text)
	case PromptStdin:
		args = append(args, "--prompt", "")
		stdin = new(request.Prompt.Text)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}
