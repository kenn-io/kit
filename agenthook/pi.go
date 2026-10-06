package agenthook

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
			// https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/package-manager.ts#L603-L640
			ConfigFilename: filepath.Join("extensions", "agenthook.js"),
			// SessionEnd fires only when another session replaces this one (new,
			// resume, fork), never on quit or reload, so a stopped Pi stays resumable.
			SupportedEvents: []Event{
				EventSessionStart, EventUserPromptSubmit, EventStop, EventSessionEnd,
			},
		},
		formatScript,
		"",
		func() (string, error) { return userDotDir(filepath.Join(".pi", "agent")) },
	)
	spec.configEnvDir = piAgentDir
	spec.eventName = piEventName
	spec.script = piExtension
	spec.checkScriptLoads = piExtensionLoads
	// Pi extension handlers run in-process; the generated extension ignores
	// command output, so control decisions have nowhere to go.
	spec.responseFormat = responseObservational
	// session_start carries a reason that maps to source except for reload, and
	// the extension reports session_shutdown only for reasons that map to one:
	// https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/extensions/types.ts#L733-L741
	// https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/extensions/types.ts#L802-L808
	spec.sessionSourceRequirement = inputOptional
	spec.sessionEndReasonRequirement = inputRequired
	return spec
}

// piEventName maps Claude events to Pi extension events:
// https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/extensions/types.ts#L911-L922
// https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/extensions/types.ts#L997-L1000
func piEventName(event Event) string {
	switch event {
	case EventSessionStart:
		return "session_start"
	case EventUserPromptSubmit:
		return "before_agent_start"
	case EventStop:
		// agent_settled fires once no retry, compaction, or queued turn follows.
		return "agent_settled"
	case EventSessionEnd:
		return "session_shutdown"
	default:
		return string(event)
	}
}

var piWindowsShellPath = regexp.MustCompile(`(?i)^/(?:mnt/|cygdrive/)?([a-z])(?:/(.*))?$`)

// piAgentDir follows Pi's normalizePath for PI_CODING_AGENT_DIR: Git Bash,
// MSYS, Cygwin, and WSL drive paths on Windows, then ~:
// https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/utils/paths.ts#L67-L101
func piAgentDir(dir string) (string, error) {
	if runtime.GOOS == "windows" && strings.HasPrefix(dir, "/") &&
		!strings.HasPrefix(dir, "//") && !strings.Contains(dir, `\`) {
		if match := piWindowsShellPath.FindStringSubmatch(dir); match != nil {
			return strings.ToUpper(match[1]) + `:\` + strings.ReplaceAll(match[2], "/", `\`), nil
		}
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") ||
		(runtime.GOOS == "windows" && strings.HasPrefix(dir, `~\`)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		rest := ""
		if len(dir) > 2 {
			rest = dir[2:]
		}
		return filepath.Join(home, rest), nil
	}
	return dir, nil
}

// promotePiReason maps Pi's reason field to Claude's. session_start reasons
// become source (Pi's /new starts a fresh session as Claude's /clear does;
// reload has no equivalent), and session_shutdown reasons for a replaced
// session become SessionEnd reasons. The extension reports no shutdown for
// quit or reload.
func promotePiReason(payload map[string]json.RawMessage) error {
	var event, reason string
	if raw, ok := payload["hook_event_name"]; ok {
		if err := json.Unmarshal(raw, &event); err != nil {
			return fmt.Errorf("field %q must be a string: %w", "hook_event_name", err)
		}
	}
	if raw, ok := payload["reason"]; ok {
		if err := json.Unmarshal(raw, &reason); err != nil {
			return fmt.Errorf("field %q must be a string: %w", "reason", err)
		}
	}
	var field, value string
	switch event {
	case "session_start":
		field = "source"
		value = map[string]string{
			"startup": string(SessionSourceStartup),
			"resume":  string(SessionSourceResume),
			"fork":    string(SessionSourceFork),
			"new":     string(SessionSourceClear),
		}[reason]
	case "session_shutdown":
		field = "reason"
		value = map[string]string{
			"new":    string(SessionEndClear),
			"resume": string(SessionEndResume),
			"fork":   string(SessionEndOther),
		}[reason]
	default:
		return nil
	}
	if value == "" {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	payload[field] = encoded
	return nil
}

// piExtensionLoads refuses an extensions directory where Pi would load only a
// package.json pi.extensions list or a root index.ts or index.js, which would
// leave the generated module silently unloaded. A manifest entry counts when
// its file exists or it names the module about to be written:
// https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/package-manager.ts#L563-L601
func piExtensionLoads(path string) error {
	dir := filepath.Dir(path)
	manifestPath := filepath.Join(dir, "package.json")
	if data, err := os.ReadFile(manifestPath); err == nil {
		var manifest struct {
			Pi struct {
				Extensions []string `json:"extensions"`
			} `json:"pi"`
		}
		// Pi strips a UTF-8 BOM before parsing:
		// https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/pi-manifest.ts#L19
		data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
		if json.Unmarshal(data, &manifest) == nil {
			listed := false
			for _, entry := range manifest.Pi.Extensions {
				resolved := entry
				if !filepath.IsAbs(resolved) {
					resolved = filepath.Join(dir, entry)
				}
				if filepath.Clean(resolved) == filepath.Clean(path) {
					return nil
				}
				if _, err := os.Stat(resolved); err == nil {
					listed = true
				}
			}
			if listed {
				return piExtensionBlocked(manifestPath, path)
			}
		}
	}
	for _, name := range []string{"index.ts", "index.js"} {
		if index := filepath.Join(dir, name); fileExists(index) {
			return piExtensionBlocked(index, path)
		}
	}
	return nil
}

func piExtensionBlocked(blocker, path string) error {
	return fmt.Errorf(
		"Pi loads only %s from %s, so it would never load %s; list %s in package.json pi.extensions or remove %s",
		filepath.Base(blocker), filepath.Dir(path), filepath.Base(path), filepath.Base(path), blocker,
	)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
