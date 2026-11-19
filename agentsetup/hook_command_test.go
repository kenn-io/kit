package agentsetup_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/agentsetup"
)

func executeHook(t *testing.T, options func(agenthook.Agent, agentsetup.HookOperation) (agenthook.InstallOptions, error), args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := agentsetup.NewHookCommand(options)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func TestHookCommandLifecycle(t *testing.T) {
	config, workspace := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	t.Setenv("HOME", workspace)
	t.Setenv("USERPROFILE", workspace)
	path := filepath.Join(config, "settings.json")
	const original = `{"unrelated":"private-setting","hooks":{"Stop":[{"hooks":[{"type":"command","command":"other-hook"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
	read := func() string {
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		return string(b)
	}
	exe := filepath.Join(workspace, "example.exe")
	require.NoError(t, os.WriteFile(exe, []byte("not an executable program"), 0o700))
	opts := agenthook.InstallOptions{ConfigPath: path, WorkspaceDir: workspace, Executable: exe, Arguments: []string{"--example-hook"}, Marker: "--example-hook", Hooks: []agenthook.Hook{{Event: agenthook.EventStop}}}
	run := func(args ...string) (string, error) {
		return executeHook(t, func(agent agenthook.Agent, op agentsetup.HookOperation) (agenthook.InstallOptions, error) {
			require.Equal(t, agenthook.AgentClaude, agent)
			if op == agentsetup.HookUninstall {
				return agenthook.InstallOptions{ConfigPath: path, Marker: opts.Marker}, nil
			}
			_, err := os.Stat(exe)
			return opts, err
		}, append([]string{"--agent", "Claude"}, args...)...)
	}

	out, err := run("status")
	require.ErrorIs(t, err, agentsetup.ErrNotReady)
	require.Contains(t, out, "missing")
	require.Equal(t, original, read())
	out, err = run("install", "--dry-run")
	require.NoError(t, err)
	require.Contains(t, out, "would update")
	require.NotContains(t, out, "private-setting")
	require.Equal(t, original, read())

	out, err = run("install")
	require.NoError(t, err)
	require.Contains(t, out, "updated")
	installed := read()
	require.Contains(t, installed, "other-hook")
	require.Contains(t, installed, opts.Marker)
	out, err = run("install")
	require.NoError(t, err)
	require.Contains(t, out, "unchanged")
	out, err = run("check")
	require.NoError(t, err)
	require.Contains(t, out, "ok")
	require.Equal(t, installed, read())

	// Removing stale hooks doesn't depend on the old executable or events.
	require.NoError(t, os.Remove(exe))
	out, err = run("uninstall", "--dry-run")
	require.NoError(t, err)
	require.Contains(t, out, "would update")
	require.Equal(t, installed, read())
	_, err = run("uninstall")
	require.NoError(t, err)
	require.JSONEq(t, original, read())
	out, err = run("uninstall")
	require.NoError(t, err)
	require.Contains(t, out, "unchanged")
}

func TestHookCommandRequiresExplicitTarget(t *testing.T) {
	rejected := errors.New("not supported by application")
	calls := 0
	options := func(agenthook.Agent, agentsetup.HookOperation) (agenthook.InstallOptions, error) {
		calls++
		return agenthook.InstallOptions{}, rejected
	}
	out, err := executeHook(t, options)
	require.NoError(t, err)
	require.Contains(t, out, "install")
	for _, args := range [][]string{
		{"install"},
		{"install", "--agent", "unknown"},
		{"install", "--agent", "claude", "--agent", "codex"},
		{"uninstall", "all"},
	} {
		_, err := executeHook(t, options, args...)
		require.Error(t, err)
	}
	require.Zero(t, calls)
	_, err = executeHook(t, options, "install", "--agent", "claude")
	require.ErrorIs(t, err, rejected)
}

func TestHookCommandNestedMountAndScope(t *testing.T) {
	active := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", active)
	projectPath := filepath.Join(t.TempDir(), "settings.json")
	run := func(args ...string) (string, error) {
		var project bool
		hook := agentsetup.NewHookCommand(func(_ agenthook.Agent, _ agentsetup.HookOperation) (agenthook.InstallOptions, error) {
			opts := agenthook.InstallOptions{
				Executable: "example", Arguments: []string{"--example-hook"}, Marker: "--example-hook",
				Hooks: []agenthook.Hook{{Event: agenthook.EventStop}},
			}
			if project {
				opts.ConfigPath = projectPath
			}
			return opts, nil
		})
		hook.Use = "hook"
		hook.PersistentFlags().BoolVar(&project, "project", false, "configure this project")
		agent := &cobra.Command{Use: "agent"}
		agent.AddCommand(hook)
		root := &cobra.Command{Use: "example", SilenceErrors: true, SilenceUsage: true}
		root.AddCommand(agent)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs(append([]string{"agent", "hook", "--project", "--agent", "claude"}, args...))
		err := root.ExecuteContext(t.Context())
		return out.String(), err
	}
	out, err := run("install", "--dry-run")
	require.NoError(t, err)
	require.Contains(t, out, projectPath)
	require.NoFileExists(t, projectPath)
	_, err = run("install")
	require.NoError(t, err)
	require.FileExists(t, projectPath)
	out, err = run("status")
	require.ErrorIs(t, err, agentsetup.ErrNotReady)
	require.Contains(t, out, "unknown")
	require.Contains(t, out, "outside the active user config")
	require.NoFileExists(t, filepath.Join(active, "settings.json"))
}

func ExampleNewHookCommand() {
	root := &cobra.Command{Use: "example"}
	hook := agentsetup.NewHookCommand(func(_ agenthook.Agent, _ agentsetup.HookOperation) (agenthook.InstallOptions, error) {
		return agenthook.InstallOptions{
			Executable: "example", Arguments: []string{"--example-session-hook"},
			Marker: "--example-session-hook", Hooks: []agenthook.Hook{{Event: agenthook.EventSessionStart}},
		}, nil
	})
	root.AddCommand(hook)
	for _, cmd := range hook.Commands() {
		fmt.Println(cmd.CommandPath())
	}
	// Output:
	// example agent-hook install
	// example agent-hook status
	// example agent-hook uninstall
}
