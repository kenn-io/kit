package agentsetup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"

	"go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/agentmcp"
)

// Hooks installs and checks one hook registration.
func Hooks(agent string, opts agenthook.InstallOptions) Item {
	a := agenthook.Agent(agent)
	_, known := agenthook.LookupProfile(a)
	item := Item{Name: "hooks", retrySetup: true, Check: func(ctx context.Context) (Status, error) {
		if !known {
			return Status{State: StateUnknown, Reason: "Hook inspection is unavailable for " + agent, NextStep: "Inspect hooks in " + agent}, nil
		}
		r, err := agenthook.Check(ctx, a, opts)
		if err != nil {
			return Status{}, err
		}
		return Status{State: State(r.State), Reason: r.Reason, NextStep: r.NextStep, ConfigPath: r.ConfigPath, HookCount: r.HookCount}, nil
	}}
	if known {
		item.Install = func(context.Context) error {
			_, err := agenthook.Install(a, opts)
			return err
		}
	}
	return item
}

// MCP installs and checks a named MCP server.
func MCP(agent, name string, server agentmcp.Server) Item {
	a := agentmcp.Agent(agent)
	_, known := agentmcp.LookupProfile(a)
	item := Item{Name: "mcp " + name, retrySetup: true, Check: func(ctx context.Context) (Status, error) {
		if !known {
			return Status{State: StateUnknown, Reason: "MCP inspection is unavailable for " + agent, NextStep: "Inspect the server in " + agent}, nil
		}
		r, err := agentmcp.Check(ctx, a, name, server)
		if err != nil {
			return Status{}, err
		}
		status := Status{State: State(r.State), Reason: r.Reason, NextStep: r.NextStep, ConfigPath: r.ConfigPath}
		if a == agentmcp.AgentCodex && r.State == agentmcp.CheckOK {
			status.ToolCount = new(r.ToolCount)
		}
		return status, nil
	}}
	if known {
		item.Install = func(context.Context) error {
			_, err := agentmcp.Install(a, agentmcp.InstallOptions{Name: name, Server: server})
			return err
		}
	}
	item.Commands = func() (agenthook.Commands, error) {
		if !known {
			return agenthook.Commands{}, fmt.Errorf("%w for agent %q", agentmcp.ErrCommandUnavailable, agent)
		}
		return agentmcp.Command(a, name, server)
	}
	return item
}

// Print writes manual MCP commands without installing or checking items.
func Print(w io.Writer, opts Options) error {
	if len(opts.Agents) == 0 {
		opts.Agents = detectedAgents()
		if len(opts.Agents) == 0 {
			opts.Agents = []string{"claude", "codex"}
		}
	}
	entries, err := resolve(opts)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.item.Commands == nil {
			continue
		}
		commands, err := e.item.Commands()
		if errors.Is(err, agentmcp.ErrCommandUnavailable) {
			// Each unavailable-command error already names its agent.
			if _, err := fmt.Fprintf(w, "# %s\n", err); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		command := commands.POSIX
		if runtime.GOOS == "windows" {
			command = commands.PowerShell
		}
		if _, err := fmt.Fprintln(w, command); err != nil {
			return err
		}
	}
	return nil
}
