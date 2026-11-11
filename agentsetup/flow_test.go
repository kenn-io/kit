package agentsetup

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/internal/codexapp/testserver"
)

func TestOrderedInstallAndFinalCheck(t *testing.T) {
	var calls []string
	report, err := Run(t.Context(), Options{Agents: []string{"codex"}, SetupCommand: "app setup", Items: func(string) ([]Item, error) {
		return []Item{
			{Name: "broken", Install: func(context.Context) error {
				calls = append(calls, "install broken")
				return errors.New("write failed")
			}, Check: func(context.Context) (Status, error) {
				calls = append(calls, "check broken")
				return Status{State: StateUntrusted, NextStep: "Open /hooks"}, nil
			}},
			{Name: "same error", Install: func(context.Context) error {
				calls = append(calls, "install same error")
				return errors.New("invalid config")
			}, Check: func(context.Context) (Status, error) {
				calls = append(calls, "check same error")
				return Status{State: StateUnknown, Reason: "invalid config", NextStep: "Correct config"}, nil
			}},
			{Name: "independent", Install: func(context.Context) error { calls = append(calls, "install independent"); return nil }, Check: func(context.Context) (Status, error) {
				calls = append(calls, "check independent")
				return Status{State: StateOK}, nil
			}},
		}, nil
	}})
	require.NoError(t, err)
	require.Equal(t, []string{"install broken", "install same error", "install independent", "check broken", "check same error", "check independent"}, calls)
	require.Equal(t, StateFailed, report.Rows[0].State)
	require.Equal(t, "write failed", report.Rows[0].Reason)
	require.Equal(t, "app setup --agent codex", report.Rows[0].NextStep)
	require.Equal(t, StateFailed, report.Rows[1].State)
	require.Equal(t, "invalid config", report.Rows[1].Reason)
	require.Equal(t, "Correct config", report.Rows[1].NextStep)
}

func TestInvalidCallbacksBeforeInstall(t *testing.T) {
	called, installed := false, false
	items := func(string) ([]Item, error) {
		called = true
		return []Item{{Name: "first", Install: func(context.Context) error { installed = true; return nil }, Check: func(context.Context) (Status, error) { return Status{State: StateOK}, nil }}, {Name: "missing check"}}, nil
	}
	for name, opts := range map[string]Options{
		"missing setup command": {Agents: []string{"claude"}, Items: items},
		"missing items":         {Agents: []string{"claude"}, SetupCommand: "app setup"},
		"missing check":         {Agents: []string{"claude"}, SetupCommand: "app setup", Items: items},
	} {
		t.Run(name, func(t *testing.T) {
			called = false
			var err error
			require.NotPanics(t, func() { _, err = Run(t.Context(), opts) })
			require.Error(t, err)
			require.False(t, installed)
			if name != "missing check" {
				require.False(t, called)
			}
		})
	}
}

func TestCancellationRetainsReadinessReport(t *testing.T) {
	for _, phase := range []string{"install", "check"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			called := false
			report, err := Run(ctx, Options{Agents: []string{"codex"}, SetupCommand: "app setup", Items: func(string) ([]Item, error) {
				if phase == "install" {
					return []Item{
						{Name: "first", Install: func(context.Context) error { cancel(); return errors.New("write failed") }, Check: func(context.Context) (Status, error) { called = true; return Status{}, nil }},
						{Name: "second", Install: func(context.Context) error { called = true; return nil }, Check: func(context.Context) (Status, error) { called = true; return Status{}, nil }},
					}, nil
				}
				return []Item{
					{Name: "checked", Check: func(context.Context) (Status, error) { return Status{State: StateOK}, nil }},
					{Name: "interrupted", Check: func(context.Context) (Status, error) { cancel(); return Status{State: StateOK}, nil }},
					{Name: "unchecked", Check: func(context.Context) (Status, error) { called = true; return Status{State: StateOK}, nil }},
				}, nil
			}})
			require.ErrorIs(t, err, context.Canceled)
			require.False(t, called)
			if phase == "install" {
				require.Equal(t, StateFailed, report.Rows[0].State)
				require.Equal(t, "write failed", report.Rows[0].Reason)
			} else {
				require.Len(t, report.Rows, 3)
				require.Equal(t, StateOK, report.Rows[0].State)
				require.Equal(t, StateUnknown, report.Rows[1].State)
				require.Equal(t, StateUnknown, report.Rows[2].State)
				require.NotZero(t, report.ExitCode())
			}
		})
	}
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
	require.NotContains(t, out.String(), "Run")
	r.Rows = make([]Row, 1)
	r.Rows[0].State, r.Rows[0].ToolCount = StateOK, new(0)
	out.Reset()
	require.NoError(t, r.WriteText(&out, ""))
	require.Contains(t, out.String(), "0 tools")
}

func TestCheckOnlyGuidance(t *testing.T) {
	installed := false
	states := []State{StateMissing, StateOutdated, StateUnknown, StateFailed}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			report, err := Run(t.Context(), Options{Agents: []string{"claude"}, SetupCommand: "app setup", CheckOnly: true, Items: func(string) ([]Item, error) {
				return []Item{
					{Name: "without step", Install: func(context.Context) error { installed = true; return nil }, Check: func(context.Context) (Status, error) { return Status{State: state}, nil }},
					{Name: "with step", retrySetup: true, Check: func(context.Context) (Status, error) { return Status{State: state, NextStep: "do this"}, nil }},
				}, nil
			}})
			require.NoError(t, err)
			require.Equal(t, "app setup --agent claude", report.Rows[0].NextStep)
			want := "do this"
			if state == StateMissing || state == StateOutdated {
				want = "app setup --agent claude"
			}
			require.Equal(t, want, report.Rows[1].NextStep)
		})
	}
	require.False(t, installed)
}

func TestReportFlattensCells(t *testing.T) {
	r := Report{Rows: []Row{{AgentName: "Claude\nCode", Piece: "mcp\tserver", State: StateFailed, Reason: "failed\nissue\rnext\vline\fform\xffend", NextStep: "app  setup\nretry"}}}
	var out bytes.Buffer
	require.NoError(t, r.WriteText(&out, ""))
	require.Equal(t, 2, strings.Count(out.String(), "\n"))
	require.Contains(t, out.String(), "failed issue next line form end: app  setup retry")
	require.Contains(t, out.String(), "Claude Code")
	require.Contains(t, out.String(), "mcp server")
	r.Rows[0].Reason = "failed\nissue\rnext"
	data, err := json.Marshal(r)
	require.NoError(t, err)
	require.Contains(t, string(data), `failed\nissue\rnext`)
}
