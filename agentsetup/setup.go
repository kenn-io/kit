package agentsetup

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"go.kenn.io/kit/agenthook"
)

// Options describes agent selection and the items each agent needs.
// Items is required. SetupCommand is required by Run and supplies missing next steps.
type Options struct {
	Agents       []string
	CheckOnly    bool
	SetupCommand string
	Items        func(agent string) ([]Item, error)
}

// Item describes one independently installed and checked piece of setup.
// Install must be idempotent and complete its item before returning nil. Check is required by Run.
type Item struct {
	Name       string
	Install    func(context.Context) error
	Check      func(context.Context) (Status, error)
	Commands   func() (agenthook.Commands, error)
	retrySetup bool
}

type entry struct {
	agent agenthook.Agent
	name  string
	item  Item
}

func normalizeAgents(names []string) ([]string, error) {
	var result []string
	for _, name := range names {
		agent, err := agenthook.ParseAgent(name)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(result, string(agent)) {
			result = append(result, string(agent))
		}
	}
	return result, nil
}

func resolve(opts Options) ([]entry, error) {
	if opts.Items == nil {
		return nil, errors.New("items callback is required")
	}
	names := opts.Agents
	if len(names) == 0 {
		names = detectedAgents()
		if len(names) == 0 {
			return nil, errors.New("no supported agent found; install Claude Code or Codex, or pass --agent")
		}
	}
	names, err := normalizeAgents(names)
	if err != nil {
		return nil, err
	}
	var entries []entry
	for _, name := range names {
		agent := agenthook.Agent(name)
		profile, _ := agenthook.LookupProfile(agent)
		items, err := opts.Items(string(agent))
		if err != nil {
			return nil, fmt.Errorf("%s items: %w", profile.DisplayName, err)
		}
		for _, item := range items {
			entries = append(entries, entry{agent, profile.DisplayName, item})
		}
	}
	return entries, nil
}

func detectedAgents() []string {
	var names []string
	for _, name := range []string{"claude", "codex"} {
		if _, err := exec.LookPath(name); err == nil {
			names = append(names, name)
		}
	}
	return names
}

func (opts Options) repairCommand() string {
	if len(opts.Agents) == 0 {
		return opts.SetupCommand
	}
	names, _ := normalizeAgents(opts.Agents)
	return opts.SetupCommand + " --agent " + strings.Join(names, ",")
}

// Run installs each item once in order, then checks final readiness. On cancellation,
// it returns completed checks and marks unchecked items unknown.
func Run(ctx context.Context, opts Options) (Report, error) {
	if strings.TrimSpace(opts.SetupCommand) == "" {
		return Report{}, errors.New("setup command is required")
	}
	entries, err := resolve(opts)
	if err != nil {
		return Report{}, err
	}
	report := Report{Rows: make([]Row, len(entries))}
	for i, e := range entries {
		if e.item.Check == nil {
			return Report{}, fmt.Errorf("%s %s: check callback is required", e.name, e.item.Name)
		}
		report.Rows[i] = Row{Agent: string(e.agent), AgentName: e.name, Piece: e.item.Name, State: StateUnknown, Reason: "Setup interrupted; readiness was not checked", NextStep: opts.repairCommand()}
	}
	for i, e := range entries {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if e.item.Install != nil && !opts.CheckOnly {
			if err := e.item.Install(ctx); err != nil {
				report.Rows[i].State, report.Rows[i].Reason = StateFailed, err.Error()
			}
		}
	}
	for i, e := range entries {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		status, err := e.item.Check(ctx)
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		if err != nil {
			status.State, status.Reason = StateFailed, err.Error()
		}
		if status.State != StateOK && (status.NextStep == "" || e.item.retrySetup && (status.State == StateMissing || status.State == StateOutdated)) {
			status.NextStep = opts.repairCommand()
		}
		if row := report.Rows[i]; row.State == StateFailed {
			if status.NextStep == "" || status.Reason == "" || !strings.Contains(row.Reason, status.Reason) {
				status.NextStep = row.NextStep
			}
			status.State, status.Reason = StateFailed, row.Reason
		}
		report.Rows[i].Status = status
	}
	return report, ctx.Err()
}
