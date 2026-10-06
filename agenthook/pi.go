package agenthook

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
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

// piAgentDir mirrors Pi's normalizePath for PI_CODING_AGENT_DIR: Git Bash,
// MSYS, Cygwin, and WSL drive paths on Windows, then ~, then file: URLs:
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
	if strings.HasPrefix(dir, "file://") {
		return fileURLPath(dir)
	}
	return dir, nil
}

// fileURLPath follows Node's fileURLToPath for the cases Pi accepts.
func fileURLPath(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	remote := parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost")
	if runtime.GOOS != "windows" {
		if remote {
			return "", fmt.Errorf("file URL host must be localhost or empty: %s", raw)
		}
		return parsed.Path, nil
	}
	if remote {
		return `\\` + parsed.Host + filepath.FromSlash(parsed.Path), nil
	}
	return filepath.FromSlash(strings.TrimPrefix(parsed.Path, "/")), nil
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

// piExtensionLoads refuses a directory whose package.json pi.extensions or
// index.ts/index.js makes Pi load only those entries, which would leave the
// generated extension silently unloaded:
// https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/package-manager.ts#L563-L601
func piExtensionLoads(path string) error {
	dir := filepath.Dir(path)
	var entries []string
	if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		var manifest struct {
			Pi struct {
				Extensions []string `json:"extensions"`
			} `json:"pi"`
		}
		if json.Unmarshal(data, &manifest) == nil {
			for _, entry := range manifest.Pi.Extensions {
				resolved := entry
				if !filepath.IsAbs(resolved) {
					resolved = filepath.Join(dir, entry)
				}
				if _, err := os.Stat(resolved); err == nil {
					entries = append(entries, resolved)
				}
			}
		}
	}
	if len(entries) == 0 {
		for _, name := range []string{"index.ts", "index.js"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				entries = []string{filepath.Join(dir, name)}
				break
			}
		}
	}
	for _, entry := range entries {
		if filepath.Clean(entry) == filepath.Clean(path) {
			return nil
		}
	}
	if len(entries) > 0 {
		return fmt.Errorf(
			"Pi loads only %s from %s, so it would never load %s; "+
				"add it to package.json pi.extensions or install to another directory",
			strings.Join(entries, ", "), dir, filepath.Base(path),
		)
	}
	return nil
}
