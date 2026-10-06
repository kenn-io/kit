package agenthook

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed pi_extension.js
var piExtension string

func piProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentPi, DisplayName: "Pi",
			ConfigEnvironment: "PI_CODING_AGENT_DIR",
			// Pi loads top-level *.js and *.ts files from <agent dir>/extensions
			// and skips dotfiles, so atomicfile's staging file is never loaded:
			// https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/src/core/package-manager.ts#L603-L640
			ConfigFilename: filepath.Join("extensions", "agenthook.js"),
			// SessionEnd is left out: Pi emits session_shutdown on SIGTERM and
			// SIGHUP, so an application stopping Pi would erase the ID it resumes:
			// https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/src/modes/interactive/interactive-mode.ts#L4258-L4270
			SupportedEvents: []Event{EventSessionStart, EventUserPromptSubmit, EventStop},
		},
		formatScript,
		"",
		func() (string, error) { return userDotDir(filepath.Join(".pi", "agent")) },
	)
	spec.configEnvDir = expandPiAgentDir
	spec.eventName = piEventName
	spec.script = piExtension
	// Pi extension handlers run in-process; the generated extension ignores
	// command output, so control decisions have nowhere to go.
	spec.responseFormat = responseObservational
	// session_start carries a reason that maps to source except for reload:
	// https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/src/core/extensions/types.ts#L733-L741
	spec.sessionSourceRequirement = inputOptional
	return spec
}

// piEventName maps Claude events to Pi extension events:
// https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/src/core/extensions/types.ts#L911-L922
// https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/src/core/extensions/types.ts#L997-L1000
func piEventName(event Event) string {
	switch event {
	case EventSessionStart:
		return "session_start"
	case EventUserPromptSubmit:
		return "before_agent_start"
	case EventStop:
		// agent_settled fires once no retry, compaction, or queued turn follows.
		return "agent_settled"
	default:
		return string(event)
	}
}

// expandPiAgentDir follows Pi's tilde expansion of PI_CODING_AGENT_DIR:
// https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/src/config.ts#L588-L611
func expandPiAgentDir(dir string) (string, error) {
	rest, ok := strings.CutPrefix(dir, "~")
	if !ok || (rest != "" && rest[0] != '/' && rest[0] != filepath.Separator) {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, rest), nil
}

// promotePiSource maps session_start's reason to Claude's source. Pi's /new
// starts a fresh session as Claude's /clear does; reload has no equivalent.
func promotePiSource(payload map[string]json.RawMessage) error {
	if _, exists := payload["source"]; exists {
		return nil
	}
	var event, reason string
	if raw, ok := payload["hook_event_name"]; ok {
		if err := json.Unmarshal(raw, &event); err != nil {
			return fmt.Errorf("field %q must be a string: %w", "hook_event_name", err)
		}
	}
	raw, ok := payload["reason"]
	if event != "session_start" || !ok {
		return nil
	}
	if err := json.Unmarshal(raw, &reason); err != nil {
		return fmt.Errorf("field %q must be a string: %w", "reason", err)
	}
	source := map[string]SessionSource{
		"startup": SessionSourceStartup,
		"resume":  SessionSourceResume,
		"fork":    SessionSourceFork,
		"new":     SessionSourceClear,
	}[reason]
	if source == "" {
		return nil
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		return err
	}
	payload["source"] = encoded
	return nil
}
