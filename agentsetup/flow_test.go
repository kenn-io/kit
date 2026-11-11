package agentsetup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/internal/codexapp/testserver"
)

func TestOrderedInstallAndFinalCheck(t *testing.T) {
	var calls []string
	ready := false
	report, err := Run(t.Context(), Options{Agents: []string{"codex"}, SetupCommand: "app setup", Items: func(string) ([]Item, error) {
		return []Item{
			{Name: "prerequisite", Install: func(context.Context) error { calls = append(calls, "install prerequisite"); ready = true; return nil }, Check: func(context.Context) (Status, error) {
				calls = append(calls, "check prerequisite")
				return Status{State: StateOK}, nil
			}},
			{Name: "dependent", Install: func(context.Context) error {
				calls = append(calls, "install dependent")
				if !ready {
					return errors.New("prerequisite missing")
				}
				return nil
			}, Check: func(context.Context) (Status, error) {
				calls = append(calls, "check dependent")
				return Status{State: StateOK}, nil
			}},
			{Name: "broken", Install: func(context.Context) error {
				calls = append(calls, "install broken")
				return errors.New("write failed")
			}, Check: func(context.Context) (Status, error) {
				calls = append(calls, "check broken")
				return Status{State: StateOK}, nil
			}},
		}, nil
	}})
	require.NoError(t, err)
	require.Equal(t, []string{"install prerequisite", "install dependent", "install broken", "check prerequisite", "check dependent", "check broken"}, calls)
	require.Equal(t, StateFailed, report.Rows[2].State)
	require.Equal(t, "write failed", report.Rows[2].Reason)
	require.Equal(t, "app setup --agent codex", report.Rows[2].NextStep)
}

func TestSetupCommandRequiredBeforeCallbacks(t *testing.T) {
	called := false
	_, err := Run(t.Context(), Options{Agents: []string{"codex"}, Items: func(string) ([]Item, error) { called = true; return nil, nil }})
	require.Error(t, err)
	require.False(t, called)
}

func TestDefaultAndExplicitSelection(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	testserver.InstallExecutable(t, "codex")
	opts := Options{SetupCommand: "app setup", CheckOnly: true, Items: func(string) ([]Item, error) {
		return []Item{{Name: "custom", Check: func(context.Context) (Status, error) { return Status{State: StateOK}, nil }}}, nil
	}}
	report, err := Run(t.Context(), opts)
	require.NoError(t, err)
	require.Len(t, report.Rows, 1)
	require.Equal(t, "codex", report.Rows[0].Agent)
	opts.Agents = []string{"claude"}
	report, err = Run(t.Context(), opts)
	require.NoError(t, err)
	require.Len(t, report.Rows, 1)
	require.Equal(t, "claude", report.Rows[0].Agent)
}

func TestReportCountsRows(t *testing.T) {
	r := Report{Rows: make([]Row, 2)}
	for i := range r.Rows {
		r.Rows[i].State = StateFailed
		r.Rows[i].NextStep = "retry setup"
	}
	var out bytes.Buffer
	require.NoError(t, r.WriteText(&out, "app setup --check"))
	require.Contains(t, out.String(), "Done except 2 steps for you. Run `app setup --check` afterwards.")
	r.Rows = append(r.Rows, r.Rows[0])
	out.Reset()
	require.NoError(t, r.WriteText(&out, ""))
	require.Contains(t, out.String(), "Done except 3 steps for you")
}

func TestCheckOnlyGuidance(t *testing.T) {
	states := []State{StateMissing, StateOutdated, StateUnknown, StateFailed}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			installed := false
			report, err := Run(t.Context(), Options{Agents: []string{"claude"}, SetupCommand: "app setup", CheckOnly: true, Items: func(string) ([]Item, error) {
				return []Item{
					{Name: "without step", Install: func(context.Context) error { installed = true; return nil }, Check: func(context.Context) (Status, error) { return Status{State: state}, nil }},
					{Name: "with step", Check: func(context.Context) (Status, error) { return Status{State: state, NextStep: "do this"}, nil }},
				}, nil
			}})
			require.NoError(t, err)
			require.False(t, installed)
			require.Equal(t, "app setup --agent claude", report.Rows[0].NextStep)
			require.Equal(t, "do this", report.Rows[1].NextStep)
		})
	}
}

func TestCancelStopsCallbacks(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	called := false
	_, err := Run(ctx, Options{Agents: []string{"codex"}, SetupCommand: "app setup", Items: func(string) ([]Item, error) {
		return []Item{
			{Name: "first", Install: func(context.Context) error { cancel(); return nil }, Check: func(context.Context) (Status, error) { called = true; return Status{}, nil }},
			{Name: "second", Install: func(context.Context) error { called = true; return nil }, Check: func(context.Context) (Status, error) { called = true; return Status{}, nil }},
		}, nil
	}})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, called)
}

func TestNativeGuidance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"disableAllHooks":true}`), 0o600))
	hook := agenthook.InstallOptions{Executable: "example", Arguments: []string{"hook"}, Marker: "hook", Hooks: []agenthook.Hook{{Event: agenthook.EventStop}}}
	report, err := Run(t.Context(), Options{Agents: []string{"claude"}, SetupCommand: "app setup", Items: func(agent string) ([]Item, error) {
		return []Item{Hooks(agent, hook)}, nil
	}})
	require.NoError(t, err)
	require.Equal(t, StateDisabled, report.Rows[0].State)
	require.NotEmpty(t, report.Rows[0].NextStep)
	require.NotEqual(t, "app setup --agent claude", report.Rows[0].NextStep)
	report, err = Run(t.Context(), Options{Agents: []string{"muse"}, SetupCommand: "app setup", Items: func(agent string) ([]Item, error) {
		return []Item{MCP(agent, "example", agentmcp.Server{Command: "example"})}, nil
	}})
	require.NoError(t, err)
	require.Equal(t, StateFailed, report.Rows[0].State)
	require.Equal(t, "Choose an agent that supports MCP", report.Rows[0].NextStep)
	t.Run("failed hook install", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		hook := Hooks("claude", agenthook.InstallOptions{Executable: "example", Arguments: []string{"example-hook"}, Marker: "example-hook", Hooks: []agenthook.Hook{{Event: agenthook.EventStop}}})
		hook.Install = func(context.Context) error { return errors.New("write failed") }
		report, err := Run(t.Context(), Options{Agents: []string{"claude"}, SetupCommand: "app setup", Items: func(string) ([]Item, error) { return []Item{hook}, nil }})
		require.NoError(t, err)
		require.Equal(t, StateFailed, report.Rows[0].State)
		require.Equal(t, "write failed", report.Rows[0].Reason)
		require.Equal(t, "app setup --agent claude", report.Rows[0].NextStep)
	})
}
