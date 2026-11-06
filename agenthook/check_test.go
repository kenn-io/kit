package agenthook

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/internal/codexapp"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("KIT_HOOK_CHECK_HELPER"); mode != "" {
		os.Exit(runHookCheckHelper(mode))
	}
	os.Exit(m.Run())
}

// Literal protocol frames protect the handshake and method names independently
// of the client's request encoder and the checker's response types.
func runHookCheckHelper(mode string) int {
	if len(os.Args) != 3 || os.Args[1] != "app-server" || os.Args[2] != "--stdio" {
		return 2
	}
	if file := os.Getenv("KIT_HOOK_CHECK_PID"); file != "" {
		if err := os.WriteFile(file, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			return 3
		}
	}
	input := bufio.NewScanner(os.Stdin)
	if !input.Scan() {
		return 4
	}
	var request struct {
		ID     int            `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(input.Bytes(), &request); err != nil {
		return 5
	}
	if request.ID != 1 || request.Method != "initialize" {
		return 6
	}
	info, _ := request.Params["clientInfo"].(map[string]any)
	caps, _ := request.Params["capabilities"].(map[string]any)
	if info["name"] != "kit-agent-check" || caps["experimentalApi"] != true {
		return 7
	}
	if mode == "initialize-eof" {
		return 0
	}
	fmt.Println(`{"method":"notice","params":{}}`)
	fmt.Println(`{"id":999,"result":{}}`)
	fmt.Println(`{"id":1,"result":{}}`)
	if !input.Scan() || string(input.Bytes()) != `{"method":"initialized","params":{}}` {
		return 8
	}
	for input.Scan() {
		if err := json.Unmarshal(input.Bytes(), &request); err != nil {
			return 9
		}
		if mode == "block" {
			signals := make(chan os.Signal, 1)
			signal.Notify(signals, os.Interrupt)
			<-signals
			signal.Stop(signals)
			return 10
		}
		if mode == "eof" {
			return 0
		}
		if mode == "rpc-error" {
			fmt.Printf("{\"id\":%d,\"error\":{\"message\":\"native failure\"}}\n", request.ID)
			continue
		}
		if mode == "no-result" {
			fmt.Printf("{\"id\":%d}\n", request.ID)
			continue
		}
		switch request.Method {
		case "config/read":
			if request.Params["includeLayers"] != false {
				return 11
			}
			feature := "true"
			if mode == "feature-disabled" {
				feature = "false"
			}
			fmt.Printf("{\"id\":%d,\"result\":{\"config\":{\"features\":{\"hooks\":%s}}}}\n", request.ID, feature)
		case "hooks/list":
			cwd, _ := os.Getwd()
			args, _ := request.Params["cwds"].([]any)
			if len(args) != 1 || args[0] != cwd {
				return 12
			}
			response := strings.ReplaceAll(os.Getenv("KIT_HOOK_CHECK_REPLY"), "CWD", cwd)
			// Paths need JSON quoting on Windows.
			if runtime.GOOS == "windows" {
				response = strings.ReplaceAll(os.Getenv("KIT_HOOK_CHECK_REPLY"), "CWD", strings.ReplaceAll(cwd, `\`, `\\`))
			}
			fmt.Printf("{\"id\":%d,\"result\":%s}\n", request.ID, response)
		default:
			return 13
		}
	}
	if mode == "block-close" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt)
		<-signals
		signal.Stop(signals)
	}
	if file := os.Getenv("KIT_HOOK_CHECK_EXIT"); file != "" {
		_ = os.WriteFile(file, []byte("EOF"), 0o600)
	}
	return 0
}

func TestCheckCodexNativeStates(t *testing.T) {
	for _, tc := range []struct {
		name, mode, trust, extra string
		want                     CheckState
	}{
		{"new hooks require review", "normal", "untrusted", "", CheckUntrusted},
		{"reviewed", "normal", "trusted", "", CheckOK},
		{"managed", "normal", "managed", "", CheckOK},
		{"other applications and project hooks", "mixed", "trusted", "", CheckOK},
		{"wrong native identity", "wrong-index", "trusted", "", CheckMissing},
		{"partial discovery", "partial", "trusted", "", CheckUnknown},
		{"modified before outdated", "normal", "modified", ",\"async\":true", CheckModified},
		{"reviewed but outdated", "normal", "trusted", ",\"async\":true", CheckOutdated},
		{"disabled handler", "disabled", "untrusted", "", CheckDisabled},
		{"disabled feature before empty discovery", "feature-disabled", "", "", CheckDisabled},
		{"unknown trust", "normal", "future", "", CheckUnknown},
		{"missing enabled", "no-enabled", "trusted", "", CheckUnknown},
		{"discovery error", "discovery-error", "trusted", "", CheckUnknown},
		{"missing discovery data", "missing-data", "", "", CheckUnknown},
		{"missing hooks field", "missing-hooks", "", "", CheckUnknown},
		{"RPC error", "rpc-error", "", "", CheckUnknown},
		{"malformed RPC", "no-result", "", "", CheckUnknown},
		{"EOF", "eof", "", "", CheckUnknown},
		{"initialization EOF", "initialize-eof", "", "", CheckUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", dir)
			executable, err := os.Executable()
			require.NoError(t, err)
			commands, err := BuildCommand(executable, "example-marker")
			require.NoError(t, err)
			commandJSON, err := json.Marshal(commands.Native)
			require.NoError(t, err)
			config := fmt.Sprintf(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":%s%s}]}]}}`, commandJSON, tc.extra)
			if tc.mode == "partial" {
				config = fmt.Sprintf(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":%s}]},{"hooks":[{"type":"command","command":%s}]}]}}`, commandJSON, commandJSON)
			}
			path := filepath.Join(dir, "hooks.json")
			require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
			enabled := `,"enabled":true`
			if tc.mode == "disabled" {
				enabled = `,"enabled":false`
			}
			if tc.mode == "no-enabled" {
				enabled = ""
			}
			pathJSON, err := json.Marshal(path)
			require.NoError(t, err)
			keyJSON, err := json.Marshal(path + ":stop:0:0")
			require.NoError(t, err)
			reply := fmt.Sprintf(`{"data":[{"cwd":"CWD","hooks":[{"source":"user","sourcePath":%s,"key":%s,"trustStatus":%q%s}],"errors":[]}]}`, pathJSON, keyJSON, tc.trust, enabled)
			if tc.mode == "mixed" {
				foreign := fmt.Sprintf(`{"source":"project","sourcePath":%s,"key":%s,"enabled":false,"trustStatus":"modified"},{"source":"user","sourcePath":%s,"key":%q,"enabled":false,"trustStatus":"modified"},`, pathJSON, keyJSON, pathJSON, path+":stop:1:0")
				reply = strings.Replace(reply, `"hooks":[`, `"hooks":[`+foreign, 1)
			}
			if tc.mode == "wrong-index" {
				reply = strings.ReplaceAll(reply, ":stop:0:0", ":stop:8:0")
			}
			if tc.mode == "discovery-error" {
				reply = `{"data":[{"cwd":"CWD","hooks":[],"errors":[{"message":"invalid hooks"}]}]}`
			}
			if tc.mode == "missing-hooks" {
				reply = `{"data":[{"cwd":"CWD","errors":[]}]}`
			}
			if tc.mode == "missing-data" {
				reply = `{}`
			}
			t.Setenv("KIT_HOOK_CHECK_HELPER", tc.mode)
			t.Setenv("KIT_HOOK_CHECK_REPLY", reply)
			result, err := Check(t.Context(), AgentCodex, CheckOptions{
				InstallOptions:   InstallOptions{Command: commands.Native, Marker: "example-marker", Hooks: []Hook{{Event: EventStop}}},
				WorkingDirectory: dir, CodexExecutable: executable,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.State, result.Reason)
			count := 1
			if tc.mode == "partial" {
				count = 2
			}
			assert.Equal(t, count, result.HookCount)
			if tc.want != CheckOK {
				assert.NotEmpty(t, result.Reason)
				assert.NotEmpty(t, result.NextStep)
			}
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, config, string(after))
		})
	}
}

func TestCheckClaudeRegistrations(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	commands, err := BuildCommand(executable, "example-marker")
	require.NoError(t, err)
	commandJSON, err := json.Marshal(commands.Native)
	require.NoError(t, err)
	// Raw commands preserve the same encoding on every platform in this table.
	handler := fmt.Sprintf(`{"type":"command","command":%s}`, commandJSON)
	for _, tc := range []struct {
		name, config string
		want         CheckState
		count        int
	}{
		{"missing", `{}`, CheckMissing, 0},
		{"current", `{"hooks":{"Stop":[{"hooks":[HANDLER]}]}}`, CheckOK, 1},
		{"foreign group after ours", `{"hooks":{"Stop":[{"hooks":[HANDLER]},{"hooks":[{"type":"command","command":"foreign"}]}]}}`, CheckOK, 1},
		{"mixed handlers", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"foreign"},HANDLER]}]}}`, CheckOK, 1},
		{"disabled", `{"disableAllHooks":true,"hooks":{"Stop":[{"hooks":[HANDLER]}]}}`, CheckDisabled, 1},
		{"duplicate", `{"hooks":{"Stop":[{"hooks":[HANDLER]},{"hooks":[HANDLER]}]}}`, CheckOutdated, 2},
		{"partial", `{"hooks":{"SessionStart":[{"hooks":[HANDLER]}]}}`, CheckOutdated, 1},
		{"group behavior", `{"hooks":{"Stop":[{"async":true,"hooks":[HANDLER]}]}}`, CheckOutdated, 1},
		{"invalid JSON", `invalid`, CheckUnknown, 0},
		{"invalid structure", `{"hooks":{"Stop":true}}`, CheckUnknown, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			path := filepath.Join(dir, "settings.json")
			config := strings.ReplaceAll(tc.config, "HANDLER", handler)
			require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
			result, err := Check(t.Context(), AgentClaude, CheckOptions{InstallOptions: InstallOptions{Command: commands.Native, Marker: "example-marker", Hooks: []Hook{{Event: EventStop}}}})
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.State, result.Reason)
			assert.Equal(t, tc.count, result.HookCount)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, config, string(after))
		})
	}
}

func TestCheckCommandResolution(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		want          CheckState
	}{
		{"unresolved PATH name", "example-missing-hook-executable example-marker", CheckUnresolved},
		{"shell expansion", "$EXAMPLE_HOOK example-marker", CheckUnknown},
		{"shell compound", "echo example-marker; echo other", CheckUnknown},
		{"glob executable", "*hook example-marker", CheckUnknown},
		{"tilde executable", "~/hook example-marker", CheckUnknown},
		{"relative executable", "./example-hook example-marker", CheckUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			opts := InstallOptions{Command: tc.command, Marker: "example-marker", Hooks: []Hook{{Event: EventStop}}}
			_, err := Install(AgentClaude, opts)
			require.NoError(t, err)
			result, err := Check(t.Context(), AgentClaude, CheckOptions{InstallOptions: opts})
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.State, result.Reason)
			assert.NotEmpty(t, result.NextStep)
		})
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	got, err := hookExecutable(map[string]any{"command": executable, "args": []any{"agent-hook", "example-marker"}})
	require.NoError(t, err)
	assert.Equal(t, executable, got)
}

func TestCheckValidationAndConfigHome(t *testing.T) {
	_, err := Check(t.Context(), AgentCodex, CheckOptions{})
	require.Error(t, err)
	options := CheckOptions{InstallOptions: InstallOptions{Command: "echo example-marker", Marker: "example-marker", Hooks: []Hook{{Event: EventStop}}}}
	t.Setenv("CODEX_HOME", t.TempDir())
	options.InstallOptions.ConfigPath = filepath.Join(t.TempDir(), "hooks.json")
	result, err := Check(t.Context(), AgentCodex, options)
	require.NoError(t, err)
	assert.Equal(t, CheckUnknown, result.State)
	assert.Contains(t, result.NextStep, "config home")
	options.InstallOptions.ConfigPath = ""
	options.CodexExecutable = filepath.Join(t.TempDir(), "missing-codex")
	result, err = Check(t.Context(), AgentCodex, options)
	require.NoError(t, err)
	assert.Equal(t, CheckUnknown, result.State)
	assert.Contains(t, result.NextStep, "PATH")
}

func TestCheckClientCancellationAndEOF(t *testing.T) {
	for _, mode := range []string{"normal", "block", "block-close", "initialize-eof"} {
		t.Run(mode, func(t *testing.T) {
			executable, err := os.Executable()
			require.NoError(t, err)
			dir := t.TempDir()
			pidFile, exitFile := filepath.Join(dir, "pid"), filepath.Join(dir, "exit")
			t.Setenv("KIT_HOOK_CHECK_HELPER", mode)
			t.Setenv("KIT_HOOK_CHECK_PID", pidFile)
			t.Setenv("KIT_HOOK_CHECK_EXIT", exitFile)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			client, startErr := codexapp.Start(ctx, executable, dir)
			if mode == "initialize-eof" {
				require.Error(t, startErr)
			} else {
				require.NoError(t, startErr)
				if mode == "block" {
					err := client.Call("config/read", map[string]any{"includeLayers": false}, nil)
					require.Error(t, err)
				}
				client.Close()
			}
			if mode == "normal" {
				data, err := os.ReadFile(exitFile)
				require.NoError(t, err)
				assert.Equal(t, "EOF", string(data))
			}
			if runtime.GOOS != "windows" {
				data, err := os.ReadFile(pidFile)
				require.NoError(t, err)
				pid, err := strconv.Atoi(string(data))
				require.NoError(t, err)
				process, err := os.FindProcess(pid)
				require.NoError(t, err)
				require.Error(t, process.Signal(syscall.Signal(0)), "the exact child must be reaped before returning")
				_ = process.Release()
			}
		})
	}
}

func TestCheckCodexConformance(t *testing.T) {
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("Codex CLI is required for native conformance")
	}
	dir := t.TempDir()
	for _, key := range []string{"HOME", "CODEX_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, dir)
	}
	configPath := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("[features]\nhooks = true\nplugins = false\n"), 0o600))
	executable, err := os.Executable()
	require.NoError(t, err)
	opts := CheckOptions{
		InstallOptions:   InstallOptions{Executable: executable, Arguments: []string{"example-marker"}, Marker: "example-marker", Hooks: []Hook{{Event: EventStop}}},
		WorkingDirectory: dir, CodexExecutable: codex,
	}
	for _, want := range []CheckState{CheckMissing, CheckUntrusted, CheckOK, CheckModified} {
		before, err := os.ReadFile(configPath)
		require.NoError(t, err)
		result, err := Check(t.Context(), AgentCodex, opts)
		require.NoError(t, err)
		after, err := os.ReadFile(configPath)
		require.NoError(t, err)
		assert.Equal(t, before, after)
		require.Equal(t, want, result.State, result.Reason)
		switch want {
		case CheckMissing:
			_, err := Install(AgentCodex, opts.InstallOptions)
			require.NoError(t, err)
		case CheckUntrusted:
			// Only this fixture records trust, through Codex's reported native hash.
			client, err := codexapp.Start(t.Context(), codex, dir)
			require.NoError(t, err)
			defer client.Close()
			var listed struct {
				Data []struct {
					Hooks []struct {
						Key  string `json:"key"`
						Hash string `json:"currentHash"`
					} `json:"hooks"`
				} `json:"data"`
			}
			require.NoError(t, client.Call("hooks/list", map[string]any{"cwds": []string{dir}}, &listed))
			require.Len(t, listed.Data, 1)
			require.Len(t, listed.Data[0].Hooks, 1)
			hook := listed.Data[0].Hooks[0]
			require.NotEmpty(t, hook.Hash)
			err = client.Call("config/batchWrite", map[string]any{"edits": []any{map[string]any{"keyPath": toml.Key{"hooks", "state", hook.Key, "trusted_hash"}.String(), "value": hook.Hash, "mergeStrategy": "replace"}}}, nil)
			client.Close()
			require.NoError(t, err)
		case CheckOK:
			path := filepath.Join(dir, "hooks.json")
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "example-marker", "example-marker changed")), 0o600))
		case CheckModified:
		default:
			require.FailNow(t, "unexpected conformance transition", string(want))
		}
	}
	// Missing CLI on PATH must remain unknown, even when hooks are present.
	t.Setenv("PATH", t.TempDir())
	opts.CodexExecutable = ""
	result, err := Check(t.Context(), AgentCodex, opts)
	require.NoError(t, err)
	assert.Equal(t, CheckUnknown, result.State)
}

func TestCheckClaudeBuiltAndQuotedExecutable(t *testing.T) {
	for _, built := range []bool{false, true} {
		t.Run(strconv.FormatBool(built), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			executable := filepath.Join(dir, "example hook.exe")
			// Executable resolution must inspect this file without attempting to run it.
			require.NoError(t, os.WriteFile(executable, []byte("not a program"), 0o700))
			opts := InstallOptions{Executable: executable, Arguments: []string{"example-marker"}, Marker: "example-marker", Hooks: []Hook{{Event: EventStop}}}
			if !built {
				commands, err := BuildCommand(executable, "example-marker")
				require.NoError(t, err)
				opts.Executable, opts.Arguments, opts.Command = "", nil, commands.Native
			}
			_, err := Install(AgentClaude, opts)
			require.NoError(t, err)
			result, err := Check(t.Context(), AgentClaude, CheckOptions{InstallOptions: opts})
			require.NoError(t, err)
			assert.Equal(t, CheckOK, result.State, result.Reason)
		})
	}
}
