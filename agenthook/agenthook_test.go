package agenthook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/atomicfile"
	"gopkg.in/yaml.v3"
)

const testMarker = "--source shared-agent-hook-test"

func TestProfilesExposeClaudeStyleEvents(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	profiles := Profiles()
	require.Len(profiles, 9)
	assert.Equal([]Agent{
		AgentClaude,
		AgentCodex,
		AgentCopilot,
		AgentCursor,
		AgentDroid,
		AgentGemini,
		AgentHermes,
		AgentPi,
		AgentQwen,
	}, []Agent{
		profiles[0].Agent,
		profiles[1].Agent,
		profiles[2].Agent,
		profiles[3].Agent,
		profiles[4].Agent,
		profiles[5].Agent,
		profiles[6].Agent,
		profiles[7].Agent,
		profiles[8].Agent,
	})
	assert.Contains(profiles[6].SupportedEvents, EventPreToolUse)
	assert.NotContains(profiles[6].SupportedEvents, EventNotification)
	assert.Equal(
		[]Event{EventSessionStart, EventUserPromptSubmit, EventStop, EventSessionEnd},
		profiles[7].SupportedEvents,
	)
	assert.Contains(profiles[8].SupportedEvents, EventPermissionRequest)
}

func TestPlanInstallDefaultsToEveryProfileEvent(t *testing.T) {
	assert := assert.New(t)
	result, err := PlanInstall(AgentDroid, InstallOptions{
		ConfigPath: filepath.Join(t.TempDir(), "hooks.json"),
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
	})

	require.NoError(t, err)
	assert.True(result.Changed)
	assert.Contains(string(result.Data), `"PreToolUse"`)
	assert.Contains(string(result.Data), `"PostToolUse"`)
	assert.Contains(string(result.Data), `"Stop"`)
}

func TestBuildCommandQuotesNativeAndWindowsArguments(t *testing.T) {
	assert := assert.New(t)
	commands, err := BuildCommand(
		"/opt/Example Agent/bin/hook",
		"agent-hook", "run", "--config", "/tmp/example config.toml",
	)

	require.NoError(t, err)
	if runtime.GOOS == "windows" {
		assert.Equal(commands.Windows, commands.Native)
	} else {
		assert.Equal(
			`'/opt/Example Agent/bin/hook' agent-hook run --config '/tmp/example config.toml'`,
			commands.Native,
		)
	}
	assert.Equal(
		`"/opt/Example Agent/bin/hook" agent-hook run --config "/tmp/example config.toml"`,
		commands.Windows,
	)
	assert.Equal(
		`& '/opt/Example Agent/bin/hook' 'agent-hook' 'run' '--config' '/tmp/example config.toml'`,
		commands.PowerShell,
	)
}

func TestBuildCommandQuotesPowerShellArguments(t *testing.T) {
	commands, err := BuildCommand(
		`C:\Program Files\hook.exe`,
		`a'b`, `$value;&`, "",
	)

	require.NoError(t, err)
	assert.Equal(t,
		`& 'C:\Program Files\hook.exe' 'a''b' '$value;&' ''`,
		commands.PowerShell,
	)
}

func TestPlanInstallBuildsCommandFromExecutable(t *testing.T) {
	result, err := PlanInstall(AgentClaude, InstallOptions{
		ConfigPath: filepath.Join(t.TempDir(), "settings.json"),
		Executable: "/opt/Example Agent/hook",
		Arguments:  []string{"agent-hook", "run", "--source", "shared-agent-hook-test"},
		Marker:     testMarker,
		Hooks:      []Hook{{Event: EventSessionStart}},
	})

	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(result.Data, &root))
	handler := root["hooks"].(map[string]any)["SessionStart"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if runtime.GOOS == "windows" {
		// A command string would reach Git Bash or PowerShell; exec form reaches neither.
		assert.Equal(t, "/opt/Example Agent/hook", handler["command"])
		assert.Equal(t, []any{"agent-hook", "run", "--source", "shared-agent-hook-test"}, handler["args"])
		// Ownership checks the joined argv, so a marker spanning quoted characters matches.
		opts := InstallOptions{
			ConfigPath: filepath.Join(t.TempDir(), "settings.json"),
			Executable: "/opt/Example Agent/hook",
			Arguments:  []string{"agent-hook", "--source", "owner's app"},
			Marker:     "--source owner's app",
			Hooks:      []Hook{{Event: EventStop}},
		}
		_, err := Install(AgentClaude, opts)
		require.NoError(t, err)
		again, err := Install(AgentClaude, opts)
		require.NoError(t, err)
		assert.False(t, again.Changed)
		return
	}
	assert.Equal(t, "'/opt/Example Agent/hook' agent-hook run --source shared-agent-hook-test", handler["command"])
	assert.NotContains(t, handler, "args")
}

func TestPlanInstallRejectsWindowsShim(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("shims need a shell only on Windows")
	}
	for _, agent := range []Agent{AgentClaude, AgentPi} {
		for _, executable := range []string{`C:\tools\hook.cmd`, `C:\tools\hook.BAT`} {
			t.Run(string(agent)+" "+executable, func(t *testing.T) {
				_, err := PlanInstall(agent, InstallOptions{
					ConfigPath: filepath.Join(t.TempDir(), "hook-config"),
					Executable: executable,
					Arguments:  []string{"agent-hook", "run", "--source", "shared-agent-hook-test"},
					Marker:     testMarker,
				})

				require.ErrorContains(t, err, "pass the executable it launches")
			})
		}
	}
}

func TestUninstallMatchesMarkerAcrossExecFormArguments(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(os.WriteFile(path, []byte(`{"hooks": {"Stop": [{"hooks": [
  {"type": "command", "command": "C:\\hook.exe", "args": ["agent-hook", "--source", "shared-agent-hook-test"]},
  {"type": "command", "command": "keep-me"}
]}]}}`), 0o600))

	result, err := Uninstall(AgentClaude, path, testMarker)

	require.NoError(err)
	assert.NotContains(string(result.Data), "agent-hook")
	assert.Contains(string(result.Data), "keep-me")
}

func TestConfigPathHonorsAgentHomes(t *testing.T) {
	tests := []struct {
		agent Agent
		env   string
		path  string
	}{
		{agent: AgentClaude, env: "CLAUDE_CONFIG_DIR", path: "settings.json"},
		{agent: AgentCodex, env: "CODEX_HOME", path: "hooks.json"},
		{agent: AgentCopilot, env: "COPILOT_HOME", path: filepath.Join("hooks", "agenthook.json")},
		{agent: AgentGemini, env: "GEMINI_CLI_HOME", path: filepath.Join(".gemini", "settings.json")},
		{agent: AgentHermes, env: "HERMES_HOME", path: "config.yaml"},
		{agent: AgentPi, env: "PI_CODING_AGENT_DIR", path: filepath.Join("extensions", "agenthook.js")},
		{agent: AgentQwen, env: "QWEN_HOME", path: "settings.json"},
	}
	for _, tt := range tests {
		t.Run(string(tt.agent), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(tt.env, dir)

			path, err := ConfigPath(tt.agent)

			require.NoError(t, err)
			assert.Equal(t, filepath.Join(dir, tt.path), path)
		})
	}
}

func TestInstallJSONPreservesOtherHooksAndReplacesOwnedHooks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	oldCommand := "/old/middleman agent-hook run " + testMarker
	require.NoError(os.WriteFile(path, []byte(`{
  "sequence": 9007199254740993,
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "keep-me"}]}],
    "SessionStart": [{"hooks": [{"type": "command", "command": "`+oldCommand+`"}]}]
  }
}`), 0o640))
	// Establish the mode being preserved regardless of the process umask.
	require.NoError(os.Chmod(path, 0o640))
	command := "/opt/middleman agent-hook run " + testMarker
	opts := InstallOptions{
		ConfigPath: path,
		Command:    command,
		CommandWindows: `C:\Program Files\middleman.exe agent-hook run ` +
			testMarker,
		Marker: testMarker,
		Hooks: []Hook{
			{Event: EventPreToolUse, Matcher: ToolBash, Timeout: 2 * time.Second},
			{Event: EventStop, Timeout: 2 * time.Second},
		},
	}

	result, err := Install(AgentCodex, opts)
	require.NoError(err)
	assert.True(result.Changed)
	info, err := os.Stat(path)
	require.NoError(err)
	if runtime.GOOS != "windows" {
		assert.Equal(os.FileMode(0o640), info.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	require.NoError(err)
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var root map[string]any
	require.NoError(decoder.Decode(&root))
	assert.Equal(json.Number("9007199254740993"), root["sequence"])
	hooks := root["hooks"].(map[string]any)
	assert.NotContains(hooks, "SessionStart")
	assert.Contains(string(data), "keep-me")
	assert.Contains(string(data), `"matcher": "^Bash$"`)
	assert.Contains(string(data), `"commandWindows"`)
	assert.Equal(2, strings.Count(string(data), `"command": "`+command+`"`))

	result, err = Install(AgentCodex, opts)
	require.NoError(err)
	assert.False(result.Changed)

	result, err = Uninstall(AgentCodex, path, testMarker)
	require.NoError(err)
	assert.True(result.Changed)
	data, err = os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(data), "keep-me")
	assert.NotContains(string(data), testMarker)
}

func TestInstallReturnsResultWhenConfigWasPublishedButNotDurable(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	original := writeAtomicFile
	writeAtomicFile = func(path string, data []byte, opts ...atomicfile.Option) error {
		if err := original(path, data, opts...); err != nil {
			return err
		}
		return fmt.Errorf("%w: injected directory sync failure", atomicfile.ErrNotDurable)
	}
	t.Cleanup(func() { writeAtomicFile = original })

	result, err := Install(AgentCodex, InstallOptions{
		ConfigPath: path,
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
		Hooks:      []Hook{{Event: EventStop}},
	})

	require.ErrorIs(err, atomicfile.ErrNotDurable)
	assert.True(t, result.Changed)
	assert.Equal(t, path, result.ConfigPath)
	data, readErr := os.ReadFile(path)
	require.NoError(readErr)
	assert.Equal(t, result.Data, data)
}

func TestPlanInstallQwenUsesClaudeEventsAndMillisecondTimeouts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	result, err := PlanInstall(AgentQwen, InstallOptions{
		ConfigPath: filepath.Join(t.TempDir(), "settings.json"),
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
		Hooks: []Hook{{
			Event: EventPreToolUse, Matcher: ToolBash, Timeout: 2 * time.Second,
		}},
	})

	require.NoError(err)
	var root map[string]any
	require.NoError(json.Unmarshal(result.Data, &root))
	hooks := root["hooks"].(map[string]any)
	entries := hooks["PreToolUse"].([]any)
	entry := entries[0].(map[string]any)
	assert.Equal("run_shell_command", entry["matcher"])
	handlers := entry["hooks"].([]any)
	handler := handlers[0].(map[string]any)
	assert.InDelta(float64(2000), handler["timeout"], 0)
}

func TestPlanInstallAcceptsWholeMillisecondTimeouts(t *testing.T) {
	for _, agent := range []Agent{AgentGemini, AgentQwen} {
		t.Run(string(agent), func(t *testing.T) {
			result, err := PlanInstall(agent, InstallOptions{
				ConfigPath: filepath.Join(t.TempDir(), "settings.json"),
				Command:    "/opt/hook " + testMarker,
				Marker:     testMarker,
				Hooks: []Hook{{
					Event: EventPreToolUse, Timeout: 1500 * time.Millisecond,
				}},
			})

			require.NoError(t, err)
			assert.Contains(t, string(result.Data), `"timeout": 1500`)
		})
	}
}

func TestPlanInstallGeminiTranslatesClaudeEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	result, err := PlanInstall(AgentGemini, InstallOptions{
		ConfigPath: filepath.Join(t.TempDir(), "settings.json"),
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
		Hooks: []Hook{
			{Event: EventUserPromptSubmit},
			{Event: EventPreToolUse, Matcher: ToolBash, Timeout: 2 * time.Second},
			{Event: EventPostToolUse},
			{Event: EventStop},
		},
	})

	require.NoError(err)
	var root map[string]any
	require.NoError(json.Unmarshal(result.Data, &root))
	hooks := root["hooks"].(map[string]any)
	assert.Contains(hooks, "BeforeAgent")
	assert.Contains(hooks, "BeforeTool")
	assert.Contains(hooks, "AfterTool")
	assert.Contains(hooks, "AfterAgent")
	assert.NotContains(hooks, "UserPromptSubmit")
	beforeTool := hooks["BeforeTool"].([]any)[0].(map[string]any)
	assert.Equal("run_shell_command", beforeTool["matcher"])
	handler := beforeTool["hooks"].([]any)[0].(map[string]any)
	assert.InDelta(float64(2000), handler["timeout"], 0)
}

func TestInstallCopilotUsesDirectEntriesAndPreservesOtherHooks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "agenthook.json")
	oldCommand := "/old/hook " + testMarker
	require.NoError(os.WriteFile(path, []byte(`{
  "version": 1,
  "future": "keep-top-level",
  "hooks": {
    "Stop": [{"type": "command", "command": "keep-me"}],
    "SessionStart": [{"type": "command", "bash": "`+oldCommand+`"}]
  }
}`), 0o600))
	command := "/opt/hook " + testMarker
	commandPowerShell := `& 'C:\Program Files\hook.exe' '--source' 'shared-agent-hook-test'`
	opts := InstallOptions{
		ConfigPath:        path,
		Command:           command,
		CommandPowerShell: commandPowerShell,
		Marker:            testMarker,
		Hooks: []Hook{
			{Event: EventPreToolUse, Matcher: ToolBash, Timeout: 2 * time.Second},
			{Event: EventStop},
		},
	}

	result, err := Install(AgentCopilot, opts)
	require.NoError(err)
	assert.True(result.Changed)
	var root map[string]any
	require.NoError(json.Unmarshal(result.Data, &root))
	assert.InDelta(float64(1), root["version"], 0)
	assert.Equal("keep-top-level", root["future"])
	hooks := root["hooks"].(map[string]any)
	assert.NotContains(hooks, "SessionStart")
	assert.Len(hooks["Stop"], 2)
	preTool := hooks["PreToolUse"].([]any)[0].(map[string]any)
	assert.Equal("command", preTool["type"])
	assert.Equal("Bash", preTool["matcher"])
	assert.Equal(command, preTool["bash"])
	assert.Equal(commandPowerShell, preTool["powershell"])
	assert.InDelta(float64(2), preTool["timeoutSec"], 0)

	result, err = Install(AgentCopilot, opts)
	require.NoError(err)
	assert.False(result.Changed)

	result, err = Uninstall(AgentCopilot, path, testMarker)
	require.NoError(err)
	assert.True(result.Changed)
	assert.Contains(string(result.Data), "keep-me")
	assert.NotContains(string(result.Data), testMarker)
}

func TestPlanInstallCopilotBuildsBashAndPowerShellCommands(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	result, err := PlanInstall(AgentCopilot, InstallOptions{
		ConfigPath: filepath.Join(t.TempDir(), "agenthook.json"),
		Executable: `C:\Program Files\hook.exe`,
		Arguments:  []string{"agent-hook", "run", "--source", "shared-agent-hook-test"},
		Marker:     testMarker,
		Hooks:      []Hook{{Event: EventSessionStart}},
	})

	require.NoError(err)
	var root map[string]any
	require.NoError(json.Unmarshal(result.Data, &root))
	entry := root["hooks"].(map[string]any)["SessionStart"].([]any)[0].(map[string]any)
	assert.Equal(
		`'C:\Program Files\hook.exe' agent-hook run --source shared-agent-hook-test`,
		entry["bash"],
	)
	assert.Equal(
		`& 'C:\Program Files\hook.exe' 'agent-hook' 'run' '--source' 'shared-agent-hook-test'`,
		entry["powershell"],
	)
}

func TestCopilotCommandSelectionKeepsBashPOSIXOnWindows(t *testing.T) {
	native, windows := profileCommands(copilotProfile(), Commands{
		Native:     `"C:\Program Files\hook.exe" run`,
		POSIX:      `'C:\Program Files\hook.exe' run`,
		Windows:    `"C:\Program Files\hook.exe" run`,
		PowerShell: `& 'C:\Program Files\hook.exe' 'run'`,
	})

	assert.Equal(t, `'C:\Program Files\hook.exe' run`, native)
	assert.Equal(t, `& 'C:\Program Files\hook.exe' 'run'`, windows)
}

func TestPlanInstallCopilotDistinguishesWin32AndPowerShellOverrides(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "agenthook.json")

	result, err := PlanInstall(AgentCopilot, InstallOptions{
		ConfigPath:     path,
		Command:        "/opt/hook " + testMarker,
		CommandWindows: `"C:\Program Files\hook.exe" --source shared-agent-hook-test`,
		Marker:         testMarker,
		Hooks:          []Hook{{Event: EventSessionStart}},
	})

	require.NoError(err)
	var root map[string]any
	require.NoError(json.Unmarshal(result.Data, &root))
	entry := root["hooks"].(map[string]any)["SessionStart"].([]any)[0].(map[string]any)
	assert.Equal("/opt/hook "+testMarker, entry["command"])
	assert.NotContains(entry, "powershell")

	result, err = PlanInstall(AgentCopilot, InstallOptions{
		ConfigPath:        path,
		Command:           "/opt/hook " + testMarker,
		CommandPowerShell: `& 'C:\Program Files\hook.exe' '--source' 'shared-agent-hook-test'`,
		Marker:            testMarker,
		Hooks:             []Hook{{Event: EventSessionStart}},
	})

	require.NoError(err)
	require.NoError(json.Unmarshal(result.Data, &root))
	entry = root["hooks"].(map[string]any)["SessionStart"].([]any)[0].(map[string]any)
	assert.Equal("/opt/hook "+testMarker, entry["bash"])
	assert.Equal(
		`& 'C:\Program Files\hook.exe' '--source' 'shared-agent-hook-test'`,
		entry["powershell"],
	)
}

func TestPlanDirectJSONRejectsUnsupportedSchemaVersions(t *testing.T) {
	for _, agent := range []Agent{AgentCopilot, AgentCursor} {
		for _, version := range []string{`2`, `"1"`, `true`} {
			t.Run(fmt.Sprintf("%s/%s", agent, version), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "hooks.json")
				require.NoError(t, os.WriteFile(path, fmt.Appendf(nil,
					`{"version":%s,"hooks":{}}`, version), 0o600))

				_, err := PlanInstall(agent, InstallOptions{
					ConfigPath: path,
					Command:    "/opt/hook " + testMarker,
					Marker:     testMarker,
					Hooks:      []Hook{{Event: EventSessionStart}},
				})

				require.Error(t, err)
				assert.ErrorContains(t, err, "version must be 1")
			})
		}
	}
}

func TestPlanDirectJSONAcceptsNumericSchemaVersionOne(t *testing.T) {
	for _, version := range []string{`1`, `1.0`, `1e0`} {
		t.Run(version, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hooks.json")
			require.NoError(t, os.WriteFile(path, fmt.Appendf(nil,
				`{"version":%s,"hooks":{}}`, version), 0o600))

			_, err := PlanInstall(AgentCursor, InstallOptions{
				ConfigPath: path,
				Command:    "/opt/hook " + testMarker,
				Marker:     testMarker,
				Hooks:      []Hook{{Event: EventSessionStart}},
			})

			require.NoError(t, err)
		})
	}
}

func TestPlanInstallCursorUsesNativeDirectEntries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	result, err := PlanInstall(AgentCursor, InstallOptions{
		ConfigPath: filepath.Join(t.TempDir(), "hooks.json"),
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
		Hooks: []Hook{
			{Event: EventUserPromptSubmit},
			{Event: EventPreToolUse, Matcher: ToolBash, Timeout: 2 * time.Second},
			{Event: EventPostToolUseFailure},
			{Event: EventStop},
		},
	})

	require.NoError(err)
	var root map[string]any
	require.NoError(json.Unmarshal(result.Data, &root))
	assert.InDelta(float64(1), root["version"], 0)
	hooks := root["hooks"].(map[string]any)
	assert.Contains(hooks, "beforeSubmitPrompt")
	assert.Contains(hooks, "postToolUseFailure")
	assert.Contains(hooks, "stop")
	beforePrompt := hooks["beforeSubmitPrompt"].([]any)[0].(map[string]any)
	assert.Equal(true, beforePrompt["failClosed"])
	preTool := hooks["preToolUse"].([]any)[0].(map[string]any)
	assert.Equal("command", preTool["type"])
	assert.Equal("Shell", preTool["matcher"])
	assert.Equal("/opt/hook "+testMarker, preTool["command"])
	assert.InDelta(float64(2), preTool["timeout"], 0)
	assert.Equal(true, preTool["failClosed"])
	stop := hooks["stop"].([]any)[0].(map[string]any)
	assert.NotContains(stop, "failClosed")
}

func TestInstallHermesTranslatesEventsAndPreservesYAML(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	oldCommand := "/old/middleman agent-hook run " + testMarker
	require.NoError(os.WriteFile(path, []byte(`# keep this operator note
model: test-model
hooks_auto_accept: false
hooks:
  pre_tool_call:
    - matcher: web_search
      command: keep-me
  transform_tool_result:
    - command: keep-transform
      future_field: keep-extra
  on_session_start:
    - command: "`+oldCommand+`"
`), 0o600))
	command := "/opt/middleman agent-hook run " + testMarker
	opts := InstallOptions{
		ConfigPath: path,
		Command:    command,
		Marker:     testMarker,
		Hooks: []Hook{
			{Event: EventPreToolUse, Matcher: ToolBash, Timeout: 2 * time.Second},
			{Event: EventStop, Timeout: 2 * time.Second},
		},
	}

	result, err := Install(AgentHermes, opts)
	require.NoError(err)
	assert.True(result.Changed)
	data, err := os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(data), "# keep this operator note")
	assert.Contains(string(data), "keep-me")
	assert.NotContains(string(data), oldCommand)

	var root map[string]any
	require.NoError(yaml.Unmarshal(data, &root))
	hooks := root["hooks"].(map[string]any)
	assert.NotContains(hooks, "on_session_start")
	transform := hooks["transform_tool_result"].([]any)[0].(map[string]any)
	assert.Equal("keep-extra", transform["future_field"])
	preTool := hooks["pre_tool_call"].([]any)
	assert.Len(preTool, 2)
	installed := preTool[1].(map[string]any)
	assert.Equal("terminal", installed["matcher"])
	assert.Equal(command, installed["command"])
	assert.Equal(2, installed["timeout"])
	stop := hooks["pre_verify"].([]any)[0].(map[string]any)
	assert.Equal(command, stop["command"])

	result, err = Install(AgentHermes, opts)
	require.NoError(err)
	assert.False(result.Changed)

	result, err = Uninstall(AgentHermes, path, testMarker)
	require.NoError(err)
	assert.True(result.Changed)
	data, err = os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(data), "keep-me")
	assert.NotContains(string(data), testMarker)
}

func TestPlanUninstallHermesPreservesUnrelatedHookNodes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(os.WriteFile(path, []byte(`hooks:
  # keep event note
  pre_tool_call:
    # keep entry note
    - matcher: terminal # keep matcher note
      command: keep-me
  post_tool_call:
    - command: keep-after
`), 0o600))

	result, err := PlanUninstall(AgentHermes, path, testMarker)

	require.NoError(err)
	assert.False(result.Changed)
	data := string(result.Data)
	assert.Contains(data, "# keep event note")
	assert.Contains(data, "# keep entry note")
	assert.Contains(data, "# keep matcher note")
	assert.Less(strings.Index(data, "pre_tool_call"), strings.Index(data, "post_tool_call"))
}

func TestPlanUninstallHermesResolvesAliasedHooks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(os.WriteFile(path, []byte(`shared_hooks: &shared_hooks
  - &owned
    command: "/opt/hook --source shared-agent-hook-test"
hooks:
  pre_tool_call: *shared_hooks
  post_tool_call:
    - *owned
`), 0o600))

	result, err := PlanUninstall(AgentHermes, path, testMarker)

	require.NoError(err)
	assert.True(result.Changed)
	var root map[string]any
	require.NoError(yaml.Unmarshal(result.Data, &root))
	hooks := root["hooks"].(map[string]any)
	assert.NotContains(hooks, "pre_tool_call")
	assert.NotContains(hooks, "post_tool_call")
}

func TestPlanInstallHermesRetainsOwnedEventNodeMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(os.WriteFile(path, []byte(`hooks:
  # keep event note
  pre_tool_call: &pre_tool_hooks
    - command: "/opt/hook --source shared-agent-hook-test" # old hook
  post_tool_call:
    - command: keep-after
`), 0o600))

	result, err := PlanInstall(AgentHermes, InstallOptions{
		ConfigPath: path,
		Command:    "/new/hook " + testMarker,
		Marker:     testMarker,
		Hooks:      []Hook{{Event: EventPreToolUse, Matcher: ToolBash}},
	})

	require.NoError(err)
	data := string(result.Data)
	assert.Contains(data, "# keep event note")
	assert.Contains(data, "&pre_tool_hooks")
	assert.Less(strings.Index(data, "pre_tool_call"), strings.Index(data, "post_tool_call"))
}

func TestNormalizeConvertsNativePayloadToClaudeShape(t *testing.T) {
	tests := []struct {
		name  string
		agent Agent
		input string
		want  map[string]any
	}{
		{
			name:  "claude payload stays canonical",
			agent: AgentClaude,
			input: `{"session_id":"s1","hook_event_name":"PreToolUse","tool_name":"Bash","future":9007199254740993}`,
			want: map[string]any{
				"session_id": "s1", "hook_event_name": "PreToolUse", "tool_name": "Bash",
				"future": json.Number("9007199254740993"),
			},
		},
		{
			name:  "cursor aliases",
			agent: AgentCursor,
			input: `{"conversation_id":"c1","generation_id":"g1","hook_event_name":"postToolUse","tool_name":"Shell","tool_input":{"command":"go test ./..."},"tool_output":{"exitCode":0}}`,
			want: map[string]any{
				"session_id": "c1", "hook_event_name": "PostToolUse", "tool_name": "Bash",
				"tool_response":   map[string]any{"exitCode": json.Number("0")},
				"conversation_id": "c1", "generation_id": "g1",
			},
		},
		{
			name:  "gemini event and response aliases",
			agent: AgentGemini,
			input: `{"session_id":"g1","hook_event_name":"AfterAgent","prompt_response":"finished","stop_hook_active":true}`,
			want: map[string]any{
				"session_id": "g1", "hook_event_name": "Stop",
				"last_assistant_message": "finished", "stop_hook_active": true,
			},
		},
		{
			name:  "hermes promotes extra tool fields",
			agent: AgentHermes,
			input: `{"session_id":"h1","hook_event_name":"post_tool_call","tool_name":"terminal","tool_input":{"command":"pwd"},"extra":{"result":"/tmp","tool_call_id":"call-1","turn_id":"turn-1","status":"ok"}}`,
			want: map[string]any{
				"session_id": "h1", "hook_event_name": "PostToolUse", "tool_name": "Bash",
				"tool_use_id": "call-1", "turn_id": "turn-1", "tool_response": "/tmp",
				"extra": map[string]any{
					"result": "/tmp", "tool_call_id": "call-1", "turn_id": "turn-1", "status": "ok",
				},
			},
		},
		{
			name:  "qwen shell tool",
			agent: AgentQwen,
			input: `{"session_id":"q1","hook_event_name":"PreToolUse","tool_name":"run_shell_command"}`,
			want: map[string]any{
				"session_id": "q1", "hook_event_name": "PreToolUse", "tool_name": "Bash",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := normalize(tt.agent, strings.NewReader(tt.input))

			require.NoError(t, err)
			decoder := json.NewDecoder(strings.NewReader(string(data)))
			decoder.UseNumber()
			var got map[string]any
			require.NoError(t, decoder.Decode(&got))
			for key, want := range tt.want {
				assert.Equal(t, want, got[key], key)
			}
		})
	}
}

func TestNormalizeKeepsCanonicalFieldsOverAliases(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	data, err := normalize(AgentCursor, strings.NewReader(`{
  "session_id":"canonical",
  "conversation_id":"native",
  "hook_event_name":"preToolUse",
  "tool_response":{"canonical":true},
  "tool_output":{"native":true}
}`))

	require.NoError(err)
	var got map[string]any
	require.NoError(json.Unmarshal(data, &got))
	assert.Equal("canonical", got["session_id"])
	assert.Equal(map[string]any{"canonical": true}, got["tool_response"])
}

func TestNormalizeRejectsInvalidInput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, err := normalize(Agent("unknown"), strings.NewReader(`{}`))
	require.Error(err)
	require.ErrorContains(err, "unsupported agent hook integration")

	_, err = normalize(AgentClaude, strings.NewReader(`{"session_id":"s1"} {}`))
	require.Error(err)
	require.ErrorContains(err, "multiple JSON values")

	_, err = normalize(AgentHermes, strings.NewReader(`{
  "session_id":"h1",
  "hook_event_name":"post_tool_call",
  "extra":[]
}`))
	require.Error(err)
	assert.ErrorContains(err, `field "extra" must be an object`)
}

func TestHermesRejectsUnsupportedClaudeStyleHooks(t *testing.T) {
	tests := []struct {
		name string
		hook Hook
		want string
	}{
		{
			name: "event",
			hook: Hook{Event: EventNotification},
			want: "does not support Notification",
		},
		{
			name: "matcher",
			hook: Hook{Event: EventStop, Matcher: ToolBash},
			want: "only supports matchers",
		},
		{
			name: "timeout",
			hook: Hook{Event: EventStop, Timeout: 301 * time.Second},
			want: "must not exceed 300 seconds",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := PlanInstall(AgentHermes, InstallOptions{
				ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
				Command:    "/opt/hook " + testMarker,
				Marker:     testMarker,
				Hooks:      []Hook{tt.hook},
			})

			require.Error(t, err)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

func TestInstallPreservesConfigSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated privileges on Windows")
	}
	assert := assert.New(t)
	require := require.New(t)
	target := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(os.WriteFile(target, []byte(`{"hooks":{}}`), 0o600))
	link := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(os.Symlink(target, link))

	_, err := Install(AgentClaude, InstallOptions{
		ConfigPath: link,
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
		Hooks:      []Hook{{Event: EventSessionStart}},
	})

	require.NoError(err)
	info, err := os.Lstat(link)
	require.NoError(err)
	assert.NotZero(info.Mode() & os.ModeSymlink)
	data, err := os.ReadFile(target)
	require.NoError(err)
	assert.Contains(string(data), testMarker)
}

func TestWriteConfigRefusesLinkSwappedInForRegularConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink may need a privilege Windows CI lacks")
	}
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	other := filepath.Join(dir, "other.json")
	require.NoError(os.WriteFile(path, []byte("{}"), 0o600))
	require.NoError(os.WriteFile(other, []byte("other"), 0o600))
	original := writeAtomicFile
	writeAtomicFile = func(path string, data []byte, opts ...atomicfile.Option) error {
		// Swap the regular config for a link after writeConfig inspected it.
		require.NoError(os.Remove(path))
		require.NoError(os.Symlink(other, path))
		return original(path, data, opts...)
	}
	t.Cleanup(func() { writeAtomicFile = original })

	err := writeConfig(path, []byte("new"))

	require.Error(err)
	data, err := os.ReadFile(other)
	require.NoError(err)
	assert.Equal(t, "other", string(data))
}

func TestConfigPathNormalizesPiAgentDirAsPiDoes(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	windows := runtime.GOOS == "windows"
	pick := func(onWindows, elsewhere string) string {
		if windows {
			return onWindows
		}
		return elsewhere
	}
	tests := []struct {
		env  string
		want string
	}{
		{env: "~", want: home},
		{env: "~/pi-agent", want: filepath.Join(home, "pi-agent")},
		{env: `~\pi-agent`, want: pick(filepath.Join(home, "pi-agent"), `~\pi-agent`)},
		{env: "/c/Users/me/pi", want: pick(`C:\Users\me\pi`, "/c/Users/me/pi")},
		{env: "/mnt/d/pi", want: pick(`D:\pi`, "/mnt/d/pi")},
		{env: "/cygdrive/e", want: pick(`E:\`, "/cygdrive/e")},
		{env: "//server/share", want: "//server/share"},
		{env: pick("file:///C:/pi/agent", "file:///pi/agent"), want: pick(`C:\pi\agent`, "/pi/agent")},
		{env: pick("file://LOCALHOST/C:/pi/agent", "file://LOCALHOST/pi/agent"), want: pick(`C:\pi\agent`, "/pi/agent")},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			t.Setenv("PI_CODING_AGENT_DIR", tt.env)

			path, err := ConfigPath(AgentPi)

			require.NoError(t, err)
			assert.Equal(t, filepath.Join(tt.want, "extensions", "agenthook.js"), path)
		})
	}
}

// piScriptHooks parses the registration block of a generated Pi extension.
func piScriptHooks(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	block, err := scriptBlock(data, path)
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(block, &root))
	hooks, _ := root["hooks"].(map[string]any)
	return hooks
}

func piCommands(hooks map[string]any, event string) []string {
	var commands []string
	entries, _ := hooks[event].([]any)
	for _, entry := range entries {
		handlers, _ := entry.(map[string]any)["hooks"].([]any)
		for _, handler := range handlers {
			fields, _ := handler.(map[string]any)
			command, _ := fields["command"].(string)
			argv := []string{command}
			args, _ := fields["args"].([]any)
			for _, arg := range args {
				argv = append(argv, arg.(string))
			}
			commands = append(commands, strings.Join(argv, " "))
		}
	}
	return commands
}

func TestInstallPiKeepsOtherApplicationsCommands(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "extensions", "agenthook.js")
	install := func(executable, source string) {
		_, err := Install(AgentPi, InstallOptions{
			ConfigPath: path,
			Executable: executable,
			Arguments:  []string{"agent-hook", "--source", source},
			Marker:     "--source " + source,
		})
		require.NoError(err)
	}

	install("/opt/a", "a-hook")
	install("/opt/b", "b-hook")
	assert.Equal(
		[]string{"/opt/a agent-hook --source a-hook", "/opt/b agent-hook --source b-hook"},
		piCommands(piScriptHooks(t, path), "session_start"),
	)

	install("/moved/a", "a-hook")
	hooks := piScriptHooks(t, path)
	assert.Equal(
		[]string{"/opt/b agent-hook --source b-hook", "/moved/a agent-hook --source a-hook"},
		piCommands(hooks, "agent_settled"),
	)
	assert.Len(piCommands(hooks, "before_agent_start"), 2)

	result, err := Uninstall(AgentPi, path, "--source a-hook")
	require.NoError(err)
	assert.True(result.Changed)
	assert.Equal(
		[]string{"/opt/b agent-hook --source b-hook"},
		piCommands(piScriptHooks(t, path), "session_start"),
	)

	result, err = Uninstall(AgentPi, path, "--source b-hook")
	require.NoError(err)
	assert.True(result.Changed)
	assert.Empty(piScriptHooks(t, path))

	result, err = Uninstall(AgentPi, filepath.Join(t.TempDir(), "missing.js"), "--source b-hook")
	require.NoError(err)
	assert.False(result.Changed)
}

func TestPlanInstallPiRefusesForeignFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agenthook.js")
	original := []byte("export default function (pi) {}\n")
	require.NoError(t, os.WriteFile(path, original, 0o600))

	_, err := Install(AgentPi, InstallOptions{
		ConfigPath: path,
		Executable: "/opt/hook",
		Arguments:  []string{"--source", "shared-agent-hook-test"},
		Marker:     testMarker,
	})

	require.ErrorContains(t, err, "not written by agenthook")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, data)
}

func TestPlanInstallPiRequiresExecutableWithoutMatchers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agenthook.js")

	_, err := PlanInstall(AgentPi, InstallOptions{
		ConfigPath: path,
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
	})
	require.ErrorContains(t, err, "need Executable and Arguments")

	_, err = PlanInstall(AgentPi, InstallOptions{
		ConfigPath: path,
		Executable: "/opt/hook",
		Arguments:  []string{"--source", "shared-agent-hook-test"},
		Marker:     testMarker,
		Hooks:      []Hook{{Event: EventStop, Matcher: ToolBash}},
	})
	require.ErrorContains(t, err, "do not support matchers")
}

func TestInstallPiKeepsArgumentThatLooksLikeBlockMarker(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "agenthook.js")
	opts := InstallOptions{
		ConfigPath: path,
		Executable: "/opt/hook",
		Arguments:  []string{scriptBlockEnd, "--source", "shared-agent-hook-test"},
		Marker:     testMarker,
	}

	_, err := Install(AgentPi, opts)
	require.NoError(err)
	result, err := Install(AgentPi, opts)
	require.NoError(err)
	assert.False(t, result.Changed)
	assert.Equal(t,
		[]string{"/opt/hook " + scriptBlockEnd + " " + testMarker},
		piCommands(piScriptHooks(t, path), "session_start"),
	)

	_, err = Uninstall(AgentPi, path, testMarker)
	require.NoError(err)
	assert.Empty(t, piScriptHooks(t, path))
}

func TestInstallPiRefusesDirectoryPiLoadsOnlyEntriesFrom(t *testing.T) {
	tests := map[string]map[string]string{
		"index.js":     {"index.js": "export default () => {};\n"},
		"index.ts":     {"index.ts": "export default () => {};\n"},
		"package.json": {"package.json": `{"pi":{"extensions":["main.js"]}}`, "main.js": ""},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for file, content := range files {
				require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600))
			}
			path := filepath.Join(dir, "agenthook.js")

			_, err := Install(AgentPi, InstallOptions{
				ConfigPath: path,
				Executable: "/opt/hook",
				Arguments:  []string{"--source", "shared-agent-hook-test"},
				Marker:     testMarker,
			})

			require.ErrorContains(t, err, "would never load agenthook.js")
			assert.NoFileExists(t, path)
		})
	}
}

func TestPiExtensionHelper(t *testing.T) {
	out := os.Getenv("KIT_AGENTHOOK_PI_HELPER_OUT")
	if out == "" {
		return
	}
	payload, err := io.ReadAll(os.Stdin)
	require.NoError(t, err)
	file, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = file.Write(append(payload, '\n'))
	require.NoError(t, err)
	require.NoError(t, file.Close())
	if strings.Contains(string(payload), "agent_settled") {
		// Ignore SIGTERM and outlive the 1s hook timeout, so only a forced kill
		// with an unconditional deadline keeps the extension from waiting.
		signal.Ignore(syscall.SIGTERM)
		<-time.After(time.Minute)
	}
}

const piExtensionDriver = `
import { pathToFileURL } from "node:url";
const extension = await import(pathToFileURL(process.argv[2]).href);
const handlers = {};
extension.default({ on: (name, handler) => { handlers[name] = handler; } });
const ctx = (mode) => ({
	mode,
	cwd: "/work",
	sessionManager: { getSessionId: () => "pi-session-1", getSessionFile: () => "/sessions/1.jsonl" },
});
const fire = async (name, event, mode) => {
	try {
		await handlers[name](event, ctx(mode));
	} catch (error) {
		console.log(error.message);
	}
};
await fire("session_start", { type: "session_start", reason: "startup" }, "tui");
await fire("session_start", { type: "session_start", reason: "startup" }, "json");
await fire("session_shutdown", { type: "session_shutdown", reason: "quit" }, "tui");
await fire("session_shutdown", { type: "session_shutdown", reason: "new" }, "tui");
await fire("agent_settled", { type: "agent_settled" }, "tui");
`

func TestPiExtensionRunsRegisteredCommand(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "agenthook.js")
	missing := filepath.Join(dir, "missing-hook")
	_, err = Install(AgentPi, InstallOptions{
		ConfigPath: path,
		Executable: missing,
		Arguments:  []string{"--source", "missing-hook"},
		Marker:     "--source missing-hook",
		Hooks:      []Hook{{Event: EventSessionStart}},
	})
	require.NoError(err)
	_, err = Install(AgentPi, InstallOptions{
		ConfigPath: path,
		Executable: os.Args[0],
		Arguments:  []string{"-test.run=^TestPiExtensionHelper$", "--", "--source", "shared-agent-hook-test"},
		Marker:     testMarker,
		Hooks: []Hook{
			{Event: EventSessionStart},
			{Event: EventSessionEnd},
			{Event: EventStop, Timeout: time.Second},
		},
	})
	require.NoError(err)
	data, err := os.ReadFile(path)
	require.NoError(err)
	module := filepath.Join(dir, "extension.mjs")
	require.NoError(os.WriteFile(module, data, 0o600))
	driver := filepath.Join(dir, "driver.mjs")
	require.NoError(os.WriteFile(driver, []byte(piExtensionDriver), 0o600))
	out := filepath.Join(dir, "payloads.jsonl")
	cmd := exec.CommandContext(t.Context(), node, driver, module)
	cmd.Env = append(os.Environ(), "KIT_AGENTHOOK_PI_HELPER_OUT="+out)

	started := time.Now()
	output, err := cmd.CombinedOutput()

	require.NoError(err, string(output))
	assert.Less(time.Since(started), 30*time.Second, "the timed-out command was not killed")
	payloads, err := os.ReadFile(out)
	require.NoError(err)
	lines := strings.Split(strings.TrimSpace(string(payloads)), "\n")
	require.Len(lines, 3)
	assert.JSONEq(`{
  "hook_event_name":"session_start",
  "session_id":"pi-session-1",
  "transcript_path":"/sessions/1.jsonl",
  "cwd":"/work",
  "reason":"startup"
}`, lines[0])
	assert.Contains(lines[1], `"reason":"new"`)
	assert.Contains(lines[2], `"agent_settled"`)
	// A failed command is reported once its event's other commands have run.
	failures := strings.Split(strings.TrimSpace(string(output)), "\n")
	require.Len(failures, 2, string(output))
	assert.Contains(failures[0], "agenthook session_start commands failed: "+missing+" --source missing-hook: could not start")
	assert.NotContains(failures[0], "TestPiExtensionHelper")
	assert.Contains(failures[1], "agenthook agent_settled commands failed: ")
	assert.Contains(failures[1], "timed out after 1s")
}
