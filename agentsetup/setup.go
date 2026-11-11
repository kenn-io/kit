package agentsetup

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
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
	nativeMCP  bool
}

type selection struct {
	agent agenthook.Agent
	name  string
	items []Item
}

func normalizeAgents(names []string) ([]string, error) {
	var result []string
	seen := make(map[agenthook.Agent]bool)
	for _, name := range names {
		agent, err := agenthook.ParseAgent(name)
		if err != nil {
			return nil, err
		}
		if !seen[agent] {
			result = append(result, string(agent))
			seen[agent] = true
		}
	}
	return result, nil
}

func defaultAgents() ([]string, error) {
	var names []string
	for _, name := range []string{"claude", "codex"} {
		if _, err := exec.LookPath(name); err == nil {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, errors.New("no supported agent found; install Claude Code or Codex, or pass --agent")
	}
	return names, nil
}

func resolve(ctx context.Context, opts Options) ([]selection, error) {
	if opts.Items == nil {
		return nil, errors.New("setup items are required")
	}
	names := opts.Agents
	if len(names) == 0 {
		var err error
		if names, err = defaultAgents(); err != nil {
			return nil, err
		}
	}
	names, err := normalizeAgents(names)
	if err != nil {
		return nil, err
	}
	var selected []selection
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
		selected = append(selected, selection{agent, profile.DisplayName, items})
	}
	return selected, nil
}

func (opts Options) repairCommand() string {
	if opts.SetupCommand == "" || len(opts.Agents) == 0 {
		return opts.SetupCommand
	}
	names, _ := normalizeAgents(opts.Agents)
	return opts.SetupCommand + " --agent " + strings.Join(names, ",")
}

func nextStep(item Item, s selection, opts Options, status Status) string {
	if item.nativeMCP && status.State == StateUnknown && (s.agent == agenthook.AgentClaude || s.agent == agenthook.AgentCodex) {
		if _, err := exec.LookPath(string(s.agent)); err != nil {
			return fmt.Sprintf("Install %s, then run `%s --check`", s.name, opts.repairCommand())
		}
	}
	if status.State != StateOK && (status.NextStep == "" || item.retrySetup && (status.State == StateMissing || status.State == StateOutdated)) {
		return opts.repairCommand()
	}
	return status.NextStep
}

func checkItem(ctx context.Context, item Item, s selection, opts Options) (Status, error) {
	status, err := item.Check(ctx)
	status.NextStep = nextStep(item, s, opts, status)
	return status, err
}

// Run installs each item once in order, then checks final readiness.
func Run(ctx context.Context, opts Options) (Report, error) {
	if strings.TrimSpace(opts.SetupCommand) == "" {
		return Report{}, errors.New("setup command is required")
	}
	selected, err := resolve(ctx, opts)
	if err != nil {
		return Report{}, err
	}
	for _, s := range selected {
		for _, item := range s.items {
			if item.Check == nil {
				return Report{}, fmt.Errorf("%s: item %q has no check", s.name, item.Name)
			}
		}
	}
	installErrors := make([][]error, len(selected))
	for i, s := range selected {
		installErrors[i] = make([]error, len(s.items))
		if opts.CheckOnly {
			continue
		}
		for j, item := range s.items {
			if err := ctx.Err(); err != nil {
				return Report{}, err
			}
			if item.Install != nil {
				installErrors[i][j] = item.Install(ctx)
			}
		}
	}
	report := Report{Rows: []Row{}}
	for i, s := range selected {
		for j, item := range s.items {
			if err := ctx.Err(); err != nil {
				return Report{}, err
			}
			status, err := checkItem(ctx, item, s, opts)
			if installErrors[i][j] != nil {
				err = installErrors[i][j]
			}
			if err != nil {
				status.State, status.Reason = StateFailed, err.Error()
			}
			if status.State != StateOK && status.NextStep == "" {
				status.NextStep = opts.repairCommand()
			}
			report.Rows = append(report.Rows, Row{Agent: string(s.agent), AgentName: s.name, Piece: item.Name, Status: status})
		}
	}
	return report, ctx.Err()
}
