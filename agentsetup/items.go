package agentsetup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"

	"go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/agentmcp"
)

// Hooks installs and checks one hook registration.
func Hooks(agent string, opts agenthook.InstallOptions) Item {
	a := agenthook.Agent(agent)
	return Item{Name: "hooks", retrySetup: true, Install: func(context.Context) error {
		_, err := agenthook.Install(a, opts)
		return err
	}, Check: func(ctx context.Context) (Status, error) {
		r, err := agenthook.Check(ctx, a, opts)
		if err != nil {
			return Status{}, err
		}
		return Status{State: State(r.State), Reason: r.Reason, NextStep: r.NextStep, ConfigPath: r.ConfigPath, HookCount: r.HookCount}, nil
	}}
}

// MCP installs and checks a named MCP server.
func MCP(agent, name string, server agentmcp.Server) Item {
	a := agentmcp.Agent(agent)
	return Item{Name: "mcp " + name, retrySetup: true, Install: func(context.Context) error {
		_, err := agentmcp.Install(a, agentmcp.InstallOptions{Name: name, Server: server})
		return err
	}, Check: func(ctx context.Context) (Status, error) {
		if _, ok := agentmcp.LookupProfile(a); !ok {
			return Status{State: StateFailed, Reason: fmt.Sprintf("unsupported MCP agent %q", agent), NextStep: "Choose an agent that supports MCP"}, nil
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
	}, Commands: func() (agenthook.Commands, error) { return agentmcp.Command(a, name, server) }}
}

// Print writes manual MCP commands without installing or checking items.
func Print(w io.Writer, opts Options) error {
	if len(opts.Agents) == 0 {
		opts.Agents = []string{"claude", "codex"}
	}
	entries, err := resolve(context.Background(), opts)
	if err != nil {
		return err
	}
	var output bytes.Buffer
	for _, e := range entries {
		if e.item.Commands == nil {
			continue
		}
		commands, err := e.item.Commands()
		if err != nil {
			return err
		}
		command := commands.POSIX
		if runtime.GOOS == "windows" {
			command = commands.PowerShell
		}
		output.WriteString(command + "\n")
	}
	_, err = output.WriteTo(w)
	return err
}
