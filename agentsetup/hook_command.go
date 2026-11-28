package agentsetup

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/atomicfile"
)

// HookOperation identifies the registration details a hook subcommand needs.
type HookOperation string

const (
	HookInstall   HookOperation = "install"
	HookStatus    HookOperation = "status"
	HookUninstall HookOperation = "uninstall"
)

// NewHookCommand returns an agent-hook command with install, status (also
// called check), and uninstall subcommands. Each requires one explicit --harness.
// Applications may rename the command or attach their own persistent scope flags.
//
// options supplies the application's registration for the selected agent and
// may reject agents the application does not support. It must not install or
// run anything: previews and checks call it too. For uninstall only ConfigPath
// and Marker are used; neither the old executable nor its arguments are needed.
func NewHookCommand(options func(agenthook.Agent, HookOperation) (agenthook.InstallOptions, error)) *cobra.Command {
	h := &hookCommand{options: options}
	group := &cobra.Command{Use: "agent-hook", Short: "Manage agent hook registrations"}
	group.PersistentFlags().StringArrayVar(&h.harnesses, "harness", nil, "coding-agent harness to configure (required)")
	group.AddCommand(
		h.mutation(HookInstall, "Register this application's hooks"),
		&cobra.Command{Use: string(HookStatus), Aliases: []string{"check"}, Short: "Check hook readiness without installing", Args: cobra.NoArgs, RunE: h.status},
		h.mutation(HookUninstall, "Remove this application's hooks"),
	)
	return group
}

type hookCommand struct {
	harnesses []string
	options   func(agenthook.Agent, HookOperation) (agenthook.InstallOptions, error)
}

func (h *hookCommand) resolve(cmd *cobra.Command, op HookOperation) (agenthook.Agent, agenthook.InstallOptions, error) {
	if err := cmd.Context().Err(); err != nil {
		return "", agenthook.InstallOptions{}, err
	}
	if len(h.harnesses) != 1 {
		return "", agenthook.InstallOptions{}, errors.New("specify exactly one --harness")
	}
	agent, err := agenthook.ParseAgent(h.harnesses[0])
	if err != nil {
		return "", agenthook.InstallOptions{}, err
	}
	if h.options == nil {
		return "", agenthook.InstallOptions{}, errors.New("hook options callback is required")
	}
	opts, err := h.options(agent, op)
	return agent, opts, err
}

func (h *hookCommand) mutation(op HookOperation, short string) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{Use: string(op), Short: short, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		agent, opts, err := h.resolve(cmd, op)
		if err != nil {
			return err
		}
		if err := cmd.Context().Err(); err != nil {
			return err
		}
		var result agenthook.Result
		switch {
		case op == HookUninstall && dryRun:
			result, err = agenthook.PlanUninstall(agent, opts.ConfigPath, opts.Marker)
		case op == HookUninstall:
			result, err = agenthook.Uninstall(agent, opts.ConfigPath, opts.Marker)
		case dryRun:
			result, err = agenthook.PlanInstall(agent, opts)
		default:
			result, err = agenthook.Install(agent, opts)
		}
		if err != nil {
			if errors.Is(err, atomicfile.ErrPublished) {
				return fmt.Errorf("%s: updated %s: %w", agent, textCell(result.ConfigPath), err)
			}
			return err
		}
		message := "unchanged"
		if result.Changed && dryRun {
			message = "would update"
		} else if result.Changed {
			message = "updated"
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s %s\n", agent, message, textCell(result.ConfigPath))
		return err
	}}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show whether the config would change without writing it")
	return cmd
}

func (h *hookCommand) status(cmd *cobra.Command, _ []string) error {
	agent, opts, err := h.resolve(cmd, HookStatus)
	if err != nil {
		return err
	}
	status, err := Hooks(string(agent), opts).Check(cmd.Context())
	if err != nil {
		return err
	}
	profile, _ := agenthook.LookupProfile(agent)
	report := Report{Rows: []Row{{Agent: string(agent), AgentName: profile.DisplayName, Piece: "hooks", Status: status}}}
	if err := report.WriteText(cmd.OutOrStdout(), ""); err != nil {
		return err
	}
	if report.ExitCode() != 0 {
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		return ErrNotReady
	}
	return nil
}
