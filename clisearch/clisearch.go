// Package clisearch finds runnable Cobra commands from task descriptions using
// local command metadata. Search never executes commands or lifecycle hooks.
package clisearch

import (
	"strings"

	"github.com/spf13/cobra"
)

// Result describes one matching command. Scores are deliberately not exposed.
type Result struct {
	Command string `json:"command"`
	Summary string `json:"summary"`
	Usage   string `json:"usage"`
}

// Response is the search output. Results is non-nil even when nothing matches.
type Response struct {
	Query   string   `json:"query"`
	Results []Result `json:"results"`
}

// Options customizes command discovery and its optional Cobra adapter.
type Options struct {
	// Synonyms adds directed query expansions to the generic synonym groups.
	// Keys and values are normalized like queries. Expansion is not recursive.
	Synonyms map[string][]string
	// Skip excludes a command, but still visits its children. Hidden and
	// deprecated subtrees, and help, completion and cli under the root, are
	// always excluded.
	// The predicate must be cheap and must not mutate the command tree.
	Skip func(*cobra.Command) bool
	// Render writes search output using the caller's existing output selectors.
	// Nil uses RenderText. NewCommand does not add a JSON selector.
	Render func(*cobra.Command, Response) error
	// UsageError wraps argument, limit and flag-parse errors from NewCommand.
	// Nil keeps plain validation errors and inherited flag-error handling.
	// Render errors are returned unchanged. Callers can replace the returned
	// command's FlagErrorFunc to retain a separate flag-parse classifier.
	UsageError func(error) error
}

const keywordsAnnotation = "go.kenn.io/kit/clisearch/keywords"

// SetKeywords replaces cmd's search keywords without changing other annotations.
// Keywords may be words or phrases. A nil command is ignored.
func SetKeywords(cmd *cobra.Command, keywords ...string) {
	if cmd == nil {
		return
	}
	if cmd.Annotations == nil {
		cmd.Annotations = make(map[string]string)
	}
	cmd.Annotations[keywordsAnnotation] = strings.Join(keywords, " ")
}
