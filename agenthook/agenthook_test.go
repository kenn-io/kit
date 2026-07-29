package agenthook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const testMarker = "--source shared-agent-hook-test"

func TestProfilesExposeClaudeStyleEvents(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	profiles := Profiles()
	require.Len(profiles, 8)
	assert.Equal([]Agent{
		AgentClaude,
		AgentCodex,
		AgentCopilot,
		AgentCursor,
		AgentDroid,
		AgentGemini,
		AgentHermes,
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
	})
	assert.Contains(profiles[6].SupportedEvents, EventPreToolUse)
	assert.NotContains(profiles[6].SupportedEvents, EventNotification)
	assert.Contains(profiles[7].SupportedEvents, EventPermissionRequest)
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
	assert.Contains(t, string(result.Data), "Example Agent")
	assert.Contains(t, string(result.Data), testMarker)
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
	assert.Equal(float64(2000), handler["timeout"])
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
	assert.Equal(float64(2000), handler["timeout"])
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
	assert.Equal(float64(1), root["version"])
	assert.Equal("keep-top-level", root["future"])
	hooks := root["hooks"].(map[string]any)
	assert.NotContains(hooks, "SessionStart")
	assert.Len(hooks["Stop"], 2)
	preTool := hooks["PreToolUse"].([]any)[0].(map[string]any)
	assert.Equal("command", preTool["type"])
	assert.Equal("Bash", preTool["matcher"])
	assert.Equal(command, preTool["bash"])
	assert.Equal(commandPowerShell, preTool["powershell"])
	assert.Equal(float64(2), preTool["timeoutSec"])

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
	assert.Equal(float64(1), root["version"])
	hooks := root["hooks"].(map[string]any)
	assert.Contains(hooks, "beforeSubmitPrompt")
	assert.Contains(hooks, "postToolUseFailure")
	assert.Contains(hooks, "stop")
	preTool := hooks["preToolUse"].([]any)[0].(map[string]any)
	assert.Equal("command", preTool["type"])
	assert.Equal("Shell", preTool["matcher"])
	assert.Equal("/opt/hook "+testMarker, preTool["command"])
	assert.Equal(float64(2), preTool["timeout"])
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
	assert.ErrorContains(err, "unsupported agent hook integration")

	_, err = normalize(AgentClaude, strings.NewReader(`{"session_id":"s1"} {}`))
	require.Error(err)
	assert.ErrorContains(err, "multiple JSON values")

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
