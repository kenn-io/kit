package agenthook

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

// Native read replies use literal frames independent of the checker's types.
func runHookCheckHelper(mode string) int {
	if len(os.Args) != 3 || os.Args[1] != "app-server" || os.Args[2] != "--stdio" {
		return 2
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
	if mode == "initialize-eof" {
		return 0
	}
	if mode == "initialize-timeout" {
		input.Scan()
		return 0
	}
	fmt.Println(`{"id":1,"result":{}}`)
	if !input.Scan() || string(input.Bytes()) != `{"method":"initialized","params":{}}` {
		return 8
	}
	for input.Scan() {
		if err := json.Unmarshal(input.Bytes(), &request); err != nil {
			return 9
		}
		if mode == "rpc-timeout" || (mode == "discovery-timeout" && request.Method == "hooks/list") {
			input.Scan()
			return 0
		}
		switch request.Method {
		case "config/read":
			cwd, _ := os.Getwd()
			if request.Params["includeLayers"] != false || request.Params["cwd"] != cwd {
				return 11
			}
			feature := `"hooks":true`
			if mode == "feature-disabled" {
				feature = `"hooks":false`
			}
			if mode == "feature-default" {
				feature = ""
			}
			fmt.Printf("{\"id\":%d,\"result\":{\"config\":{\"features\":{%s}}}}\n", request.ID, feature)
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

	return 0
}

func TestCheckCodexNativeStates(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	bin := t.TempDir()
	name := "codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	program, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bin, name), program, 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		name, mode, trust, extra string
		want                     CheckState
		reason                   string
	}{
		{"new hooks require review", "normal", "untrusted", "", CheckUntrusted, ""},
		{"default hooks feature", "feature-default", "untrusted", "", CheckUntrusted, ""},
		{"reviewed", "normal", "trusted", "", CheckOK, ""},
		{"managed", "normal", "managed", "", CheckOK, ""},
		{"other applications and source files", "mixed", "trusted", "", CheckOK, ""},
		{"symlinked native source", "symlink", "untrusted", "", CheckUntrusted, ""},
		{"wrong native identity", "wrong-index", "trusted", "", CheckUnknown, "did not discover installed hooks"},
		{"partial discovery", "partial", "trusted", "", CheckUnknown, ""},
		{"modified before outdated", "normal", "modified", ",\"async\":true", CheckModified, ""},
		{"reviewed but outdated", "normal", "trusted", ",\"async\":true", CheckOutdated, ""},
		{"outdated before untrusted", "normal", "untrusted", ",\"async\":true", CheckOutdated, ""},
		{"disabled handler", "disabled", "untrusted", "", CheckDisabled, ""},
		{"disabled feature before empty discovery", "feature-disabled", "", "", CheckDisabled, ""},
		{"unknown trust", "normal", "future", "", CheckUnknown, ""},
		{"discovery error", "discovery-error", "trusted", "", CheckUnknown, "invalid hooks"},
		{"missing discovery data", "missing-data", "", "", CheckUnknown, ""},
		{"initialization EOF", "initialize-eof", "", "", CheckUnknown, ""},
		{"initialization timeout", "initialize-timeout", "", "", CheckUnknown, "context deadline exceeded"},
		{"config timeout", "rpc-timeout", "", "", CheckUnknown, "context deadline exceeded"},
		{"discovery timeout", "discovery-timeout", "", "", CheckUnknown, "context deadline exceeded"},
		{"missing CLI on PATH", "missing-cli", "", "", CheckUnknown, "codex"},
		{"missing hooks without CLI", "missing-hooks-cli", "", "", CheckMissing, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", dir)
			if tc.mode == "symlink" {
				link := filepath.Join(t.TempDir(), "linked-home")
				if err := os.Symlink(dir, link); err != nil {
					if runtime.GOOS == "windows" {
						t.Skip("symlink creation needs privileges")
					}
					require.NoError(t, err)
				}
				t.Setenv("CODEX_HOME", link)
			}
			commands, err := BuildCommand(executable, "example-marker")
			require.NoError(t, err)
			opts := InstallOptions{Command: commands.POSIX, Marker: "example-marker", Hooks: []Hook{{Event: EventStop}}}
			commandJSON, err := json.Marshal(opts.Command)
			require.NoError(t, err)
			config := fmt.Sprintf(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":%s%s}]}]}}`, commandJSON, tc.extra)
			if tc.mode == "partial" {
				config = fmt.Sprintf(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":%s}]},{"hooks":[{"type":"command","command":%s}]}]}}`, commandJSON, commandJSON)
			}
			if tc.mode == "mixed" {
				config = fmt.Sprintf(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":%s}]},{"hooks":[{"type":"command","command":"other-app-hook"}]}]}}`, commandJSON)
			}
			if tc.mode == "missing-hooks-cli" {
				config = `{}`
			}
			path := filepath.Join(dir, "hooks.json")
			require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
			enabled := `,"enabled":true`
			if tc.mode == "disabled" {
				enabled = `,"enabled":false`
			}
			pathJSON, err := json.Marshal(path)
			require.NoError(t, err)
			keyJSON, err := json.Marshal(path + ":stop:0:0")
			require.NoError(t, err)
			reply := fmt.Sprintf(`{"data":[{"cwd":"CWD","hooks":[{"source":"user","sourcePath":%s,"key":%s,"trustStatus":%q%s}],"errors":[]}]}`, pathJSON, keyJSON, tc.trust, enabled)
			if tc.mode == "mixed" {
				inlinePath := filepath.Join(dir, "config.toml")
				foreign, err := json.Marshal([]map[string]any{
					{"source": "project", "sourcePath": path, "key": path + ":stop:0:0", "enabled": true, "trustStatus": "untrusted"},
					{"source": "user", "sourcePath": path, "key": path + ":stop:1:0", "enabled": true, "trustStatus": "untrusted"},
					{"source": "user", "sourcePath": inlinePath, "key": inlinePath + ":stop:0:0", "enabled": true, "trustStatus": "untrusted"},
				})
				require.NoError(t, err)
				reply = strings.Replace(reply, `"hooks":[`, `"hooks":[`+string(foreign[1:len(foreign)-1])+",", 1)
			}
			if tc.mode == "wrong-index" {
				reply = strings.ReplaceAll(reply, ":stop:0:0", ":stop:8:0")
			}
			if tc.mode == "discovery-error" {
				reply = `{"data":[{"cwd":"CWD","hooks":[],"errors":[{"message":"invalid hooks"}]}]}`
			}
			if tc.mode == "missing-data" {
				reply = `{}`
			}
			t.Setenv("KIT_HOOK_CHECK_HELPER", tc.mode)
			t.Setenv("KIT_HOOK_CHECK_REPLY", reply)
			if strings.Contains(tc.mode, "cli") {
				t.Setenv("PATH", t.TempDir())
			}
			ctx := t.Context()
			if strings.HasSuffix(tc.mode, "timeout") {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
			}
			result, err := Check(ctx, AgentCodex, CheckOptions{
				InstallOptions: opts,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.State, result.Reason)
			if tc.reason != "" {
				assert.Contains(t, result.Reason, tc.reason)
			}
			if strings.HasSuffix(tc.mode, "timeout") {
				assert.Contains(t, result.NextStep, "more time")
			}
			if tc.mode == "initialize-eof" {
				assert.Contains(t, result.NextStep, "app-server startup")
			}
			if tc.mode == "wrong-index" {
				assert.Contains(t, result.NextStep, "effective feature settings")
			}
			if tc.mode == "missing-cli" {
				assert.Contains(t, result.NextStep, "Install Codex")
			}
			if tc.want == CheckOK {
				assert.Contains(t, result.Reason, "checking process environment")
			}
			count := 1
			if tc.mode == "partial" {
				count = 2
			}
			if tc.mode == "missing-hooks-cli" {
				count = 0
			}
			assert.Equal(t, count, result.HookCount)
			if tc.want != CheckOK {
				assert.NotEmpty(t, result.Reason)
				assert.NotEmpty(t, result.NextStep)
			}
		})
	}
}

func TestCheckClaudeRegistrations(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	commands, err := BuildCommand(executable, "example-marker")
	require.NoError(t, err)
	commandJSON, err := json.Marshal(commands.POSIX)
	require.NoError(t, err)
	// Raw commands preserve the same encoding on every platform in this table.
	handler := fmt.Sprintf(`{"type":"command","command":%s}`, commandJSON)
	for _, tc := range []struct {
		name, config string
		want         CheckState
		count        int
	}{
		{"current", `{"hooks":{"Stop":[{"hooks":[HANDLER]}]}}`, CheckOK, 1},
		{"empty matcher", `{"hooks":{"Stop":[{"matcher":"","hooks":[HANDLER]}]}}`, CheckOK, 1},
		{"foreign group after ours", `{"hooks":{"Stop":[{"hooks":[HANDLER]},{"hooks":[{"type":"command","command":"foreign"}]}]}}`, CheckOK, 1},
		{"mixed handlers", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"foreign"},HANDLER]}]}}`, CheckOK, 1},
		{"disabled", `{"disableAllHooks":true,"hooks":{"Stop":[{"hooks":[HANDLER]}]}}`, CheckDisabled, 1},
		{"duplicate", `{"hooks":{"Stop":[{"hooks":[HANDLER]},{"hooks":[HANDLER]}]}}`, CheckOutdated, 2},
		{"invalid JSON", `invalid`, CheckUnknown, 0},
		{"invalid structure", `{"hooks":{"Stop":true}}`, CheckUnknown, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			path := filepath.Join(dir, "settings.json")
			config := strings.ReplaceAll(tc.config, "HANDLER", handler)
			require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
			opts := InstallOptions{Command: commands.POSIX, Marker: "example-marker", Hooks: []Hook{{Event: EventStop}}}
			result, err := Check(t.Context(), AgentClaude, CheckOptions{InstallOptions: opts})
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.State, result.Reason)
			if tc.want == CheckOK {
				assert.Contains(t, result.Reason, "checking process environment")
			}
			assert.Equal(t, tc.count, result.HookCount)
		})
	}
}

func TestCheckCommandResolution(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("PATH", filepath.Dir(executable))
	literalExecutable := filepath.Join(t.TempDir(), "example (stable).exe")
	require.NoError(t, os.WriteFile(literalExecutable, []byte("not a program"), 0o700))
	quoted, err := BuildCommand(literalExecutable, "example-marker")
	require.NoError(t, err)
	for _, tc := range []struct {
		name         string
		options      InstallOptions
		want         CheckState
		reason, step string
	}{
		{"bare executable", InstallOptions{Command: filepath.Base(executable) + " example-marker"}, CheckOK, "", ""},
		{"quoted executable", InstallOptions{Command: quoted.POSIX}, CheckOK, "", ""},
		{"Windows command override", InstallOptions{Command: "example-missing-hook-executable example-marker", CommandWindows: quoted.Windows}, CheckOK, "", ""},
		{"unresolved PATH name", InstallOptions{Command: "example-missing-hook-executable example-marker"}, CheckUnknown, "checking process environment", "PATH"},
		{"shell assignment", InstallOptions{Command: "EXAMPLE_HOOK=1 example-hook example-marker"}, CheckUnknown, "shell interpretation", ""},
		{"shell expansion", InstallOptions{Command: "$EXAMPLE_HOOK example-marker"}, CheckUnknown, "shell interpretation", ""},
		{"shell compound", InstallOptions{Command: filepath.Base(executable) + " example-marker; echo other"}, CheckUnknown, "shell interpretation", ""},
		{"quoted expansion", InstallOptions{Command: `"$EXAMPLE_HOOK" example-marker`}, CheckUnknown, "shell interpretation", ""},
		{"glob executable", InstallOptions{Command: "*hook example-marker"}, CheckUnknown, "shell interpretation", ""},
		{"tilde executable", InstallOptions{Command: "~/hook example-marker"}, CheckUnknown, "shell interpretation", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := AgentClaude
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			opts := tc.options
			opts.Marker, opts.Hooks = "example-marker", []Hook{{Event: EventStop}}
			if opts.CommandWindows != "" {
				if runtime.GOOS != "windows" {
					t.Skip("CommandWindows is selected on Windows")
				}
				agent = AgentCodex
				t.Setenv("CODEX_HOME", dir)
				program, err := os.ReadFile(executable)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "codex.exe"), program, 0o700))
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				t.Setenv("KIT_HOOK_CHECK_HELPER", "normal")
				path := filepath.Join(dir, "hooks.json")
				pathJSON, err := json.Marshal(path)
				require.NoError(t, err)
				keyJSON, err := json.Marshal(path + ":stop:0:0")
				require.NoError(t, err)
				t.Setenv("KIT_HOOK_CHECK_REPLY", fmt.Sprintf(`{"data":[{"cwd":"CWD","hooks":[{"source":"user","sourcePath":%s,"key":%s,"trustStatus":"trusted","enabled":true}],"errors":[]}]}`, pathJSON, keyJSON))
			}
			_, err := Install(agent, opts)
			require.NoError(t, err)
			result, err := Check(t.Context(), agent, CheckOptions{InstallOptions: opts})
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.State, result.Reason)
			if tc.want != CheckOK {
				assert.NotEmpty(t, result.NextStep)
			}
			if tc.reason != "" {
				assert.Contains(t, result.Reason, tc.reason)
			}
			if tc.step != "" {
				assert.Contains(t, result.NextStep, tc.step)
			}
		})
	}
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
	assert.Contains(t, result.Reason, "outside the active user config")
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
	require.NoError(t, os.WriteFile(configPath, []byte("[features]\nplugins = false\n"), 0o600))
	executable, err := os.Executable()
	require.NoError(t, err)
	opts := CheckOptions{
		InstallOptions: InstallOptions{Executable: executable, Arguments: []string{"example-marker"}, Marker: "example-marker", Hooks: []Hook{{Event: EventStop}}},
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

	_, err = Install(AgentCodex, opts.InstallOptions)
	require.NoError(t, err)
	workspace := filepath.Join(dir, "workspace")
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, ".codex"), 0o700))
	t.Chdir(workspace)
	for _, userEnabled := range []bool{true, false} {
		config := fmt.Sprintf("[features]\nhooks = %t\nplugins = false\n[%s]\ntrust_level = \"trusted\"\n", userEnabled, toml.Key{"projects", workspace}.String())
		require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))
		workspaceConfig := fmt.Sprintf("[features]\nhooks = %t\n", !userEnabled)
		require.NoError(t, os.WriteFile(filepath.Join(workspace, ".codex", "config.toml"), []byte(workspaceConfig), 0o600))
		result, err := Check(t.Context(), AgentCodex, opts)
		require.NoError(t, err)
		want := CheckUntrusted
		if userEnabled {
			want = CheckDisabled
		}
		assert.Equal(t, want, result.State, result.Reason)
	}
}
