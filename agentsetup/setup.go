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
// SetupCommand is required by Run and supplies missing next steps.
type Options struct {
	Agents       []string
	CheckOnly    bool
	SetupCommand string
	Items        func(agent string) ([]Item, error)
}

// Item describes one independently installed and checked piece of setup.
// Install must be idempotent and complete its item before returning nil.
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

func resolve(ctx context.Context, opts Options) ([]entry, error) {
	names := opts.Agents
	if len(names) == 0 {
		for _, name := range []string{"claude", "codex"} {
			if _, err := exec.LookPath(name); err == nil {
				names = append(names, name)
			}
		}
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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

func (opts Options) repairCommand() string {
	if len(opts.Agents) == 0 {
		return opts.SetupCommand
	}
	names, _ := normalizeAgents(opts.Agents)
	return opts.SetupCommand + " --agent " + strings.Join(names, ",")
}

func nextStep(e entry, opts Options, status Status) string {
	if status.State != StateOK && (status.NextStep == "" || e.item.retrySetup && (status.State == StateMissing || status.State == StateOutdated)) {
		return opts.repairCommand()
	}
	return status.NextStep
}

// Run installs each item once in order, then checks final readiness.
func Run(ctx context.Context, opts Options) (Report, error) {
	if strings.TrimSpace(opts.SetupCommand) == "" {
		return Report{}, errors.New("setup command is required")
	}
	entries, err := resolve(ctx, opts)
	if err != nil {
		return Report{}, err
	}
	installErrors := make([]error, len(entries))
	for i, e := range entries {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		if e.item.Install != nil && !opts.CheckOnly {
			installErrors[i] = e.item.Install(ctx)
		}
	}
	report := Report{Rows: []Row{}}
	for i, e := range entries {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		status, err := e.item.Check(ctx)
		status.NextStep = nextStep(e, opts, status)
		if installErrors[i] != nil {
			installReason := installErrors[i].Error()
			if err != nil || status.NextStep == "" || status.Reason == "" || !strings.Contains(installReason, status.Reason) {
				status.NextStep = opts.repairCommand()
			}
			status.State, status.Reason = StateFailed, installReason
		} else if err != nil {
			status.State, status.Reason = StateFailed, err.Error()
			if status.NextStep == "" {
				status.NextStep = opts.repairCommand()
			}
		}
		report.Rows = append(report.Rows, Row{Agent: string(e.agent), AgentName: e.name, Piece: e.item.Name, Status: status})
	}
	return report, ctx.Err()
}
