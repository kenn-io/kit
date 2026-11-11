package agentsetup

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"

	"github.com/spf13/cobra"
)

// ErrNotReady indicates that the report contains an item that is not ready.
var ErrNotReady = errors.New("agent setup is not ready")

// NewCommand returns a setup command ready to attach to an application.
func NewCommand(setup Options) *cobra.Command {
	var jsonOutput, printOnly bool
	cmd := &cobra.Command{Use: "setup", Short: "Set up agent integrations", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if printOnly {
			return Print(cmd.OutOrStdout(), setup)
		}
		if setup.SetupCommand == "" {
			setup.SetupCommand = cmd.CommandPath()
		}
		report, err := Run(cmd.Context(), setup)
		if err != nil {
			return err
		}
		if jsonOutput {
			err = json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), report)
		} else {
			err = report.WriteText(cmd.OutOrStdout(), setup.repairCommand()+" --check")
		}
		if err != nil {
			return err
		}
		if report.ExitCode() != 0 {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			return ErrNotReady
		}
		return nil
	}}
	cmd.Flags().StringSliceVar(&setup.Agents, "agent", setup.Agents, "agent to configure (repeatable)")
	cmd.Flags().BoolVar(&setup.CheckOnly, "check", setup.CheckOnly, "check without installing")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	cmd.Flags().BoolVar(&printOnly, "print", false, "print manual MCP commands")
	return cmd
}
