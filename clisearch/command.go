package clisearch

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// NewCommand returns an unattached cli group containing search. Attach it with
// root.AddCommand. The search command joins task words and accepts --limit 1–20
// (default 5). It inherits lifecycle hooks; callers must exempt it from any
// configuration, authentication or service initialization they own.
//
// Validation precedes command pre-run hooks, but not Cobra global initializers.
// Query preserves the joined arguments, including outer whitespace. Output is
// sent to Options.Render, or RenderText when nil.
func NewCommand(root *cobra.Command, opts Options) *cobra.Command {
	usageError := func(err error) error {
		if err != nil && opts.UsageError != nil {
			return opts.UsageError(err)
		}
		return err
	}
	group := &cobra.Command{
		Use:         "cli",
		Short:       "Discover commands for a task",
		Annotations: map[string]string{adapterAnnotation: "true"},
		Args:        func(cmd *cobra.Command, args []string) error { return usageError(cobra.NoArgs(cmd, args)) },
		RunE:        func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	limit := 5
	search := &cobra.Command{
		Use:   "search <task words...>",
		Short: "Find commands by describing a task",
		Args: func(_ *cobra.Command, args []string) error {
			if strings.TrimSpace(strings.Join(args, " ")) == "" {
				return usageError(errors.New("search requires a non-empty task description"))
			}
			if limit < 1 || limit > 20 {
				return usageError(errors.New("limit must be between 1 and 20"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.Join(args, " ")
			response := Response{Query: query, Results: Search(root, query, limit, opts)}
			render := opts.Render
			if render == nil {
				render = RenderText
			}
			return render(cmd, response)
		},
	}
	search.Flags().IntVar(&limit, "limit", 5, "maximum results (1–20)")
	if opts.UsageError != nil {
		wrap := func(_ *cobra.Command, err error) error { return usageError(err) }
		group.SetFlagErrorFunc(wrap)
		search.SetFlagErrorFunc(wrap)
	}
	group.AddCommand(search)
	return group
}

// RenderText writes compact result lines and a help hint to cmd.OutOrStdout.
// The first result supplies the hint; an empty result points to root help.
// It can be called from an Options.Render callback for the caller's text mode.
func RenderText(cmd *cobra.Command, response Response) error {
	w := cmd.OutOrStdout()
	hint := cmd.Root().CommandPath()
	for i, result := range response.Results {
		if _, err := fmt.Fprintf(w, "%s  %s\n", result.Command, result.Summary); err != nil {
			return err
		}
		if i == 0 {
			hint = result.Command
		}
	}
	_, err := fmt.Fprintf(w, "Run '%s --help' for flags and examples.\n", hint)
	return err
}
