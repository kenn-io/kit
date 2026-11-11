package agenthook

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
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

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gittest "go.kenn.io/kit/git/test"
	"go.kenn.io/kit/internal/codexapp"
	"go.kenn.io/kit/internal/codexapp/testserver"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("KIT_HOOK_CHECK_HELPER"); mode != "" {
		os.Exit(runHookCheckHelper(mode))
	}
	os.Exit(m.Run())
}

// Native read replies use literal frames independent of the checker's types.
func runHookCheckHelper(mode string) int {
	input := bufio.NewScanner(os.Stdin)
	if record := os.Getenv("KIT_HOOK_CHECK_LAUNCHES"); record != "" {
		f, err := os.OpenFile(record, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return 14
		}
		info, err := f.Stat()
		if err != nil {
			return 15
		}
		_, _ = f.WriteString("x")
		_ = f.Close()
		if mode == "different-failures" {
			if info.Size() == 0 {
				mode = "initialize-eof"
			} else {
				mode = "config-eof"
			}
		}
		if strings.HasPrefix(mode, "first-") {
			if info.Size() > 0 {
				mode = "normal"
			} else {
				mode = strings.TrimPrefix(mode, "first-")
			}
		}
	}
	if mode == "initialize-eof" || mode == "cancel-pause" {
		_, _ = testserver.Read(input)
		if mode == "cancel-pause" {
			_ = os.WriteFile(os.Getenv("KIT_HOOK_CHECK_EVENT"), []byte("ready"), 0o600)
		}
		return 0
	}
	if err := testserver.Handshake(input, os.Stdout); err != nil {
		return 6
	}
	for {
		request, err := testserver.Read(input)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 9
		}
		if mode == "truncated" {
			fmt.Print(`{"id":1,"result":`)
			return 0
		}
		if (mode == "config-no-result" && request.Method == "config/read") || (mode == "discovery-no-result" && request.Method == "hooks/list") {
			fmt.Printf("{\"id\":%d}\n", request.ID)
			continue
		}
		if mode == "rpc-error" {
			fmt.Printf("{\"id\":%d,\"error\":{\"message\":\"native failure\"}}\n", request.ID)
			continue
		}
		if mode == "malformed" {
			fmt.Println(`{"id":1,"result":}`)
			return 0
		}
		if (mode == "config-eof" && request.Method == "config/read") || (mode == "discovery-eof" && request.Method == "hooks/list") {
			return 0
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
			if mode == "close-delay" {
				input.Scan()
				_ = os.WriteFile(os.Getenv("KIT_HOOK_CHECK_EVENT"), []byte("ready"), 0o600)
				signals := make(chan os.Signal, 1)
				signal.Notify(signals, os.Interrupt)
				<-signals
				signal.Stop(signals)
				return 0
			}
		default:
			return 13
		}
	}

	return 0
}

func TestCheckCodexNativeStates(t *testing.T) {
	executable := testserver.InstallExecutable(t, "codex")
	for _, tc := range []struct {
		name, mode, trust, extra string
		want                     CheckState
		reason                   string
		launches                 int
	}{
		{"config missing result", "config-no-result", "", "", CheckUnknown, "missing result", 1},
		{"discovery missing result", "discovery-no-result", "", "", CheckUnknown, "missing result", 1},
		{"retry initialization", "first-initialize-eof", "trusted", "", CheckOK, "", 2},
		{"retry config", "first-config-eof", "trusted", "", CheckOK, "", 2},
		{"retry truncated reply", "first-truncated", "trusted", "", CheckOK, "", 2},
		{"last failure step", "different-failures", "", "", CheckUnknown, "EOF", 2},
		{"retry discovery", "first-discovery-eof", "trusted", "", CheckOK, "", 2},
		{"retry timeout", "first-rpc-timeout", "trusted", "", CheckOK, "", 2},
		{"exhausted config", "config-eof", "", "", CheckUnknown, "EOF", 2},
		{"native error is terminal", "rpc-error", "", "", CheckUnknown, "native failure", 1},
		{"malformed response is terminal", "malformed", "", "", CheckUnknown, "", 1},
		{"completed before cancellation", "close-delay", "trusted", "", CheckOK, "", 1},
		{"already canceled", "already-canceled", "", "", CheckUnknown, "context canceled", 0},
		{"cancel after startup failure", "cancel-pause", "", "", CheckUnknown, "context canceled", 1},
		{"new hooks require review", "normal", "untrusted", "", CheckUntrusted, "", 1},
		{"default hooks feature", "feature-default", "untrusted", "", CheckUntrusted, "", 1},
		{"reviewed", "normal", "trusted", "", CheckOK, "", 1},
		{"native source capitalization", "source-case", "trusted", "", CheckOK, "", 1},
		{"managed", "normal", "managed", "", CheckOK, "", 1},
		{"other applications and source files", "mixed", "trusted", "", CheckOK, "", 1},
		{"symlinked native source", "symlink", "untrusted", "", CheckUntrusted, "", 1},
		{"wrong native identity", "wrong-index", "trusted", "", CheckUnknown, "didn't list all", 1},
		{"partial discovery", "partial", "trusted", "", CheckUnknown, "didn't list all", 1},
		{"modified before outdated", "normal", "modified", ",\"async\":true", CheckModified, "", 1},
		{"reviewed but outdated", "normal", "trusted", ",\"async\":true", CheckOutdated, "", 1},
		{"outdated before untrusted", "normal", "untrusted", ",\"async\":true", CheckOutdated, "", 1},
		{"disabled handler", "disabled", "untrusted", "", CheckDisabled, "", 1},
		{"disabled feature before empty discovery", "feature-disabled", "", "", CheckDisabled, "", 1},
		{"unknown trust", "normal", "future", "", CheckUnknown, "unknown hook trust state", 1},
		{"discovery error", "discovery-error", "trusted", "", CheckUnknown, "invalid hooks", 1},
		{"missing discovery data", "missing-data", "", "", CheckUnknown, "didn't report hooks for this folder", 1},
		{"initialization EOF", "initialize-eof", "", "", CheckUnknown, "EOF", 2},
		{"config timeout", "rpc-timeout", "", "", CheckUnknown, "context deadline exceeded", 1},
		{"discovery timeout", "discovery-timeout", "", "", CheckUnknown, "context deadline exceeded", 1},
		{"missing CLI on PATH", "missing-cli", "", "", CheckUnknown, "codex", 0},
		{"Windows command override", "windows-command", "trusted", "", CheckOK, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mode == "windows-command" && runtime.GOOS != "windows" {
				t.Skip("CommandWindows is selected on Windows")
			}
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
			extra := tc.extra
			if tc.mode == "windows-command" {
				literal := filepath.Join(dir, "example (stable).exe")
				require.NoError(t, os.WriteFile(literal, []byte("not a program"), 0o700))
				command, err := BuildCommand(literal, "example-marker")
				require.NoError(t, err)
				opts.Command = "example-missing-hook-executable example-marker"
				opts.CommandWindows = command.Windows
				encoded, err := json.Marshal(opts.CommandWindows)
				require.NoError(t, err)
				extra += `,"commandWindows":` + string(encoded)
			}
			commandJSON, err := json.Marshal(opts.Command)
			require.NoError(t, err)
			config := fmt.Sprintf(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":%s%s}]}]}}`, commandJSON, extra)
			if tc.mode == "partial" {
				config = fmt.Sprintf(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":%s}]},{"hooks":[{"type":"command","command":%s}]}]}}`, commandJSON, commandJSON)
			}
			if tc.mode == "mixed" {
				config = fmt.Sprintf(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":%s}]},{"hooks":[{"type":"command","command":"other-app-hook"}]}]}}`, commandJSON)
			}
			path := filepath.Join(dir, "hooks.json")
			require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
			enabled := `,"enabled":true`
			if tc.mode == "disabled" {
				enabled = `,"enabled":false`
			}
			source := path
			if tc.mode == "source-case" {
				alternate := filepath.Join(dir, "HOOKS.json")
				originalInfo, err := os.Stat(path)
				require.NoError(t, err)
				alternateInfo, err := os.Stat(alternate)
				if os.IsNotExist(err) {
					t.Skip("filesystem treats capitalization as distinct")
				}
				require.NoError(t, err)
				require.True(t, os.SameFile(originalInfo, alternateInfo))
				source = alternate
			}
			pathJSON, err := json.Marshal(source)
			require.NoError(t, err)
			keyJSON, err := json.Marshal(source + ":stop:0:0")
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
			record := filepath.Join(dir, "launches")
			t.Setenv("KIT_HOOK_CHECK_LAUNCHES", record)
			event := filepath.Join(dir, "event")
			t.Setenv("KIT_HOOK_CHECK_EVENT", event)
			t.Setenv("KIT_HOOK_CHECK_HELPER", tc.mode)
			t.Setenv("KIT_HOOK_CHECK_REPLY", reply)
			if tc.mode == "missing-cli" {
				t.Setenv("PATH", t.TempDir())
			}
			ctx := t.Context()
			if tc.mode == "already-canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if tc.mode == "rpc-timeout" || tc.mode == "discovery-timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
			}
			started := time.Now()
			var result CheckResult
			if tc.mode == "cancel-pause" || tc.mode == "close-delay" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				finished := make(chan struct{})
				go func() {
					result, err = Check(ctx, AgentCodex, opts)
					close(finished)
				}()
				require.Eventually(t, func() bool { _, err := os.Stat(event); return err == nil }, 30*time.Second, 10*time.Millisecond)
				cancel()
				select {
				case <-finished:
				case <-time.After(30 * time.Second):
					require.FailNow(t, "cancellation must finish the check")
				}
			} else {
				result, err = Check(ctx, AgentCodex, opts)
			}
			if tc.mode == "rpc-timeout" || tc.mode == "discovery-timeout" {
				assert.Less(t, time.Since(started), 3*time.Second, "the caller deadline must bound the native check")
			}
			require.NoError(t, err)
			if tc.mode == "close-delay" {
				assert.ErrorIs(t, ctx.Err(), context.Canceled)
			}
			if tc.launches == 0 {
				_, err := os.Stat(record)
				assert.ErrorIs(t, err, os.ErrNotExist)
			} else {
				launches, err := os.ReadFile(record)
				require.NoError(t, err)
				assert.Len(t, launches, tc.launches)
			}
			if tc.mode == "first-rpc-timeout" {
				assert.Less(t, time.Since(started), 21*time.Second)
			}
			if tc.mode == "config-eof" || tc.mode == "different-failures" || tc.mode == "config-no-result" {
				assert.Contains(t, result.NextStep, "Codex config")
			}
			if tc.mode == "discovery-no-result" {
				assert.Contains(t, result.NextStep, "hooks in Codex")
			}
			assert.Equal(t, tc.want, result.State, result.Reason)
			if tc.reason != "" {
				assert.Contains(t, result.Reason, tc.reason)
			}
			if tc.mode == "initialize-eof" {
				assert.Contains(t, result.NextStep, "app-server startup")
			}
			if tc.mode == "missing-cli" {
				assert.Contains(t, result.NextStep, "Install Codex")
			}
			if tc.want == CheckOK {
				assert.Contains(t, result.Reason, "this process can find their command")
			}
			count := 1
			if tc.mode == "partial" {
				count = 2
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
	t.Chdir(t.TempDir())
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
		{"missing before disabled", `{"disableAllHooks":true}`, CheckMissing, 0},
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
			result, err := Check(t.Context(), AgentClaude, opts)
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.State, result.Reason)
			if tc.want == CheckOK {
				assert.Contains(t, result.Reason, "this process can find their command")
			}
			assert.Equal(t, tc.count, result.HookCount)
		})
	}
}

func TestCheckClaudeSettingsPrecedence(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, tc := range []struct {
		name, user, project, local string
		want                       CheckState
	}{
		{"project disables", `{}`, `{"disableAllHooks":true}`, "", CheckDisabled},
		{"project enables", `{"disableAllHooks":true}`, `{"disableAllHooks":false}`, "", CheckOK},
		{"local disables", `{}`, `{"disableAllHooks":false}`, `{"disableAllHooks":true}`, CheckDisabled},
		{"local enables", `{}`, `{"disableAllHooks":true}`, `{"disableAllHooks":false}`, CheckOK},
		{"unset keeps user", `{"disableAllHooks":true}`, `{}`, `{}`, CheckDisabled},
		{"user string", `{"disableAllHooks":"true"}`, `{}`, "", CheckUnknown},
		{"user null", `{"disableAllHooks":null}`, `{}`, "", CheckUnknown},
		{"invalid project", `{}`, `invalid`, "", CheckUnknown},
		{"invalid local", `{}`, `{}`, `invalid`, CheckUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, workspace := t.TempDir(), t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			t.Chdir(workspace)
			require.NoError(t, os.Mkdir(filepath.Join(workspace, ".claude"), 0o700))
			for path, data := range map[string]string{
				filepath.Join(dir, "settings.json"):                        tc.user,
				filepath.Join(workspace, ".claude", "settings.json"):       tc.project,
				filepath.Join(workspace, ".claude", "settings.local.json"): tc.local,
			} {
				if data != "" {
					require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
				}
			}
			opts := InstallOptions{Executable: executable, Arguments: []string{"example-marker"}, Marker: "example-marker", Hooks: []Hook{{Event: EventStop}}}
			_, err := Install(AgentClaude, opts)
			require.NoError(t, err)
			result, err := Check(t.Context(), AgentClaude, opts)
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.State, result.Reason)
		})
	}
}

func TestCheckClaudeLocalSettingsRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Claude keeps local settings in the starting directory on Windows")
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, tc := range []struct {
		name                  string
		linked, home, foreign bool
		want                  CheckState
	}{{"subdirectory", false, false, false, CheckDisabled}, {"linked", true, false, false, CheckDisabled}, {"home", false, true, false, CheckOK}, {"foreign-owned", false, false, true, CheckDisabled}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := gittest.NewRepoWithCommit(t)
			rootValue, localValue := `{"disableAllHooks":true}`, `{"disableAllHooks":false}`
			if tc.foreign {
				rootValue, localValue = localValue, rootValue
			}
			repo.WriteFile(".claude/settings.local.json", rootValue)
			if tc.foreign {
				rootSettings := filepath.Join(repo.Root, ".claude")
				uid := os.Geteuid()
				if err := os.Chown(rootSettings, uid+1, -1); os.IsPermission(err) {
					t.Skip("changing fixture ownership needs privileges")
				} else {
					require.NoError(t, err)
				}
				t.Cleanup(func() { require.NoError(t, os.Chown(rootSettings, uid, -1)) })
			}
			workspace := repo.Root
			if tc.linked {
				workspace = repo.AddWorktree("example")
			}
			if tc.home {
				t.Setenv("HOME", repo.Root)
			}
			workspace = filepath.Join(workspace, "subdir")
			require.NoError(t, os.MkdirAll(filepath.Join(workspace, ".claude"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(workspace, ".claude", "settings.local.json"), []byte(localValue), 0o600))
			t.Chdir(workspace)
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			opts := InstallOptions{Executable: executable, Arguments: []string{"example-marker"}, Marker: "example-marker", Hooks: []Hook{{Event: EventStop}}}
			_, err := Install(AgentClaude, opts)
			require.NoError(t, err)
			result, err := Check(t.Context(), AgentClaude, opts)
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.State, result.Reason)
		})
	}
}

func TestCheckCommandResolution(t *testing.T) {
	t.Chdir(t.TempDir())
	executable, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("PATH", filepath.Dir(executable))
	for _, tc := range []struct {
		name         string
		options      InstallOptions
		want         CheckState
		reason, step string
	}{
		{"bare executable", InstallOptions{Command: filepath.Base(executable) + " example-marker"}, CheckOK, "", ""},
		{"structured bare executable", InstallOptions{Executable: filepath.Base(executable), Arguments: []string{"example-marker"}}, CheckOK, "", ""},
		{"unresolved PATH name", InstallOptions{Command: "example-missing-hook-executable example-marker"}, CheckUnknown, "Can't find a usable hook command", "PATH"},
		{"shell compound", InstallOptions{Command: filepath.Base(executable) + " example-marker; echo other"}, CheckUnknown, "shell interpretation", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			opts := tc.options
			opts.Marker, opts.Hooks = "example-marker", []Hook{{Event: EventStop}}
			_, err := Install(AgentClaude, opts)
			require.NoError(t, err)
			result, err := Check(t.Context(), AgentClaude, opts)
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
	t.Run("invalid options", func(t *testing.T) {
		_, err := Check(t.Context(), AgentCodex, InstallOptions{})
		require.ErrorContains(t, err, "agent hook command is required")
	})
	options := InstallOptions{Command: "echo example-marker", Marker: "example-marker", Hooks: []Hook{{Event: EventStop}}}
	t.Run("outside active config", func(t *testing.T) {
		t.Setenv("CODEX_HOME", t.TempDir())
		options := options
		options.ConfigPath = filepath.Join(t.TempDir(), "hooks.json")
		result, err := Check(t.Context(), AgentCodex, options)
		require.NoError(t, err)
		assert.Equal(t, CheckUnknown, result.State)
		assert.Contains(t, result.Reason, "outside the active user config")
	})
	t.Run("capitalization", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("CODEX_HOME", dir)
		active := filepath.Join(dir, "hooks.json")
		alternate := filepath.Join(dir, "HOOKS.json")
		require.NoError(t, os.WriteFile(active, []byte(`{}`), 0o600))
		file, err := os.OpenFile(alternate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		want, reason := CheckUnknown, "outside the active user config"
		if os.IsExist(err) {
			originalInfo, err := os.Stat(active)
			require.NoError(t, err)
			alternateInfo, err := os.Stat(alternate)
			require.NoError(t, err)
			require.True(t, os.SameFile(originalInfo, alternateInfo))
			want, reason = CheckMissing, "This application's hooks are not installed"
		} else {
			require.NoError(t, err)
			require.NoError(t, file.Close())
		}
		options := options
		options.ConfigPath = alternate
		result, err := Check(t.Context(), AgentCodex, options)
		require.NoError(t, err)
		assert.Equal(t, want, result.State)
		assert.Contains(t, result.Reason, reason)
	})
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
	t.Chdir(dir)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	configPath := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("[features]\nplugins = false\n"), 0o600))
	executable, err := os.Executable()
	require.NoError(t, err)
	opts := InstallOptions{Executable: executable, Arguments: []string{"example-marker"}, Marker: "example-marker", Hooks: []Hook{{Event: EventStop}}}
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
			_, err := Install(AgentCodex, opts)
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

	_, err = Install(AgentCodex, opts)
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

func TestTransientCodexDeadline(t *testing.T) {
	assert.True(t, transientCodexError(fmt.Errorf("read: %w", context.DeadlineExceeded)))
	assert.True(t, transientCodexError(&os.PathError{Op: "fork/exec", Err: syscall.ETXTBSY}))
}
