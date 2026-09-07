package agentcli

import "fmt"

// NewCursor returns a Cursor Agent adapter after validating configured
// options. A zero Command uses "agent".
func NewCursor(command Command) (Adapter, error) {
	return newAdapter(Cursor, command, "agent", cursorOptionGrammar, cursorCapabilities, buildCursor)
}

var cursorOptionGrammar = optionGrammar{
	"--api-key": value("api-key"), "-H": value("header"), "--header": value("header"),
	"--stream-partial-output": flag("stream-partial-output"), "--model": value("model"),
	"--sandbox": value("sandbox"), "--approve-mcps": flag("approve-mcps"), "--trust": flag("trust"),
	"--workspace": value("workspace"), "--add-dir": value("add-dir"), "--plugin-dir": value("plugin-dir"),
	"-p": forbidden("print mode is selected by Request.Mode"), "--print": forbidden("print mode is selected by Request.Mode"),
	"--output-format": forbidden("output format belongs in Request.OutputFormat"), "--mode": forbidden("execution mode belongs in Request.Approval"),
	"--plan": forbidden("execution mode belongs in Request.Approval"), "-f": forbidden("permission bypass belongs in Request.Approval"),
	"--force": forbidden("permission bypass belongs in Request.Approval"), "--yolo": forbidden("permission bypass belongs in Request.Approval"),
	"--resume": forbidden("resume selects a session"), "--continue": forbidden("continue selects a session"),
	"--list-models": forbidden("list-models is an action, not a launch option"),
	"-w":            forbidden("worktree creation is a caller concern"), "--worktree": forbidden("worktree creation is a caller concern"),
	"--worktree-base": forbidden("worktree creation is a caller concern"), "--skip-worktree-setup": forbidden("worktree creation is a caller concern"),
	"-h": forbidden("help is an action, not a launch option"), "--help": forbidden("help is an action, not a launch option"),
	"-v": forbidden("version is an action, not a launch option"), "--version": forbidden("version is an action, not a launch option"),
}

var cursorCapabilities = Capabilities{
	Modes: []Mode{NonInteractive}, Resume: true,
	OutputFormats: []OutputFormat{OutputText, OutputJSON, OutputJSONL}, Model: true,
	ApprovalModes: []ApprovalMode{ApprovalNever, ApprovalBypass},
}

func buildCursor(a *adapter, sessionID string, request Request) (Invocation, error) {
	args := append(a.base(), "--print")
	if request.OutputFormat != OutputDefault && request.OutputFormat != OutputText {
		format := string(request.OutputFormat)
		if request.OutputFormat == OutputJSONL {
			format = "stream-json"
		}
		args = append(args, "--output-format", format)
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	switch request.Approval {
	case ApprovalNever:
		args = append(args, "--mode", "plan")
	case ApprovalBypass:
		args = append(args, "--force")
	}
	stdin, err := stdinPrompt(request.Prompt)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Cursor, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}
