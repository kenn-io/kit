package agentcli

import "fmt"

// NewKilo returns a Kilo adapter after validating configured global options.
// A zero Command uses "kilo".
func NewKilo(command Command) (Adapter, error) {
	return newAdapter(Kilo, command, "kilo", kiloOptionGrammar, kiloCapabilities, buildKilo)
}

var kiloOptionGrammar = optionGrammar{
	"--print-logs": flag("print-logs"), "--log-level": value("log-level"),
	"-m": value("model"), "--model": value("model"), "--agent": value("agent"),
	"-c": forbidden("continue selects a session"), "--continue": forbidden("continue selects a session"),
	"-s": forbidden("session selects a session"), "--session": forbidden("session selects a session"),
	"--fork": forbidden("fork changes session identity"), "--cloud-fork": forbidden("cloud-fork changes session identity"),
	"--prompt": forbidden("prompt belongs in Request.Prompt"), "--auto": forbidden("automatic permission approval belongs in Request.Approval"),
	"-h": forbidden("help is an action, not a launch option"), "--help": forbidden("help is an action, not a launch option"),
	"-v": forbidden("version is an action, not a launch option"), "--version": forbidden("version is an action, not a launch option"),
}

var kiloCapabilities = Capabilities{
	Modes:           []Mode{NonInteractive},
	Resume:          true,
	OutputFormats:   []OutputFormat{OutputText, OutputJSONL},
	Model:           true,
	ReasoningLevels: []ReasoningLevel{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMaximum},
	ApprovalModes:   []ApprovalMode{ApprovalBypass},
}

func buildKilo(a *adapter, sessionID string, request Request) (Invocation, error) {
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
	if request.Approval == ApprovalBypass {
		args = append(args, "--auto")
	}
	if variant := reasoningValue(request.Reasoning); variant != "" {
		args = append(args, "--variant", variant)
	}
	stdin, err := stdinPrompt(request.Prompt)
	if err != nil {
		return Invocation{}, fmt.Errorf("build %s invocation: %w", Kilo, err)
	}
	return Invocation{Argv: args, Stdin: stdin}, nil
}
