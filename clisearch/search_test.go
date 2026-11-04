package clisearch_test

import (
	"math"
	"slices"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/clisearch"
)

func runnable(use, summary string) *cobra.Command {
	return &cobra.Command{Use: use, Short: summary, Run: func(*cobra.Command, []string) {}}
}

func paths(results []clisearch.Result) []string {
	values := make([]string, len(results))
	for i, result := range results {
		values[i] = result.Command
	}
	return values
}

func TestSearchFields(t *testing.T) {
	for _, tc := range []struct {
		name, query, want string
		build             func(*cobra.Command)
	}{
		{"name beats summary", "review", "tool review", func(root *cobra.Command) { root.AddCommand(runnable("review", ""), runnable("aaa", "review")) }},
		{"alias", "erase", "tool delete", func(root *cobra.Command) {
			cmd := runnable("delete", "")
			cmd.Aliases = []string{"erase"}
			root.AddCommand(cmd)
		}},
		{"keyword", "annotate", "tool comment", func(root *cobra.Command) {
			cmd := runnable("comment", "")
			clisearch.SetKeywords(cmd, "annotate")
			root.AddCommand(cmd)
		}},
		{"flag name", "recipient", "tool send", func(root *cobra.Command) {
			cmd := runnable("send", "")
			cmd.Flags().String("recipient", "", "")
			root.AddCommand(cmd)
		}},
		{"flag usage", "destination", "tool send", func(root *cobra.Command) {
			cmd := runnable("send", "")
			cmd.Flags().String("to", "", "destination")
			root.AddCommand(cmd)
		}},
		{"inherited flag", "workspace", "tool group show", func(root *cobra.Command) {
			group := &cobra.Command{Use: "group"}
			group.PersistentFlags().String("workspace", "", "")
			group.AddCommand(runnable("show", ""))
			root.AddCommand(group)
		}},
		{"generic synonym", "remove", "tool delete", func(root *cobra.Command) { root.AddCommand(runnable("delete", "")) }},
		{"prefix", "migrat", "tool migrate", func(root *cobra.Command) { root.AddCommand(runnable("migrate", "")) }},
		{"case punctuation", "REVIEW!!!", "tool review", func(root *cobra.Command) { root.AddCommand(runnable("review", "")) }},
		{"plural", "comments", "tool comment", func(root *cobra.Command) { root.AddCommand(runnable("comment", "")) }},
		{"add gerund", "adding", "tool add", func(root *cobra.Command) { root.AddCommand(runnable("add", "")) }},
		{"add past tense", "added", "tool add", func(root *cobra.Command) { root.AddCommand(runnable("add", "")) }},
		{"ing suffix", "running", "tool run", func(root *cobra.Command) { root.AddCommand(runnable("run", "")) }},
		{"ed suffix", "created", "tool create", func(root *cobra.Command) { root.AddCommand(runnable("create", "")) }},
		{"ies suffix", "queries", "tool query", func(root *cobra.Command) { root.AddCommand(runnable("query", "")) }},
		{"es suffix", "boxes", "tool box", func(root *cobra.Command) { root.AddCommand(runnable("box", "")) }},
		{"keyword beats flag", "annotate", "tool comment", func(root *cobra.Command) {
			a := runnable("comment", "")
			clisearch.SetKeywords(a, "annotate")
			b := runnable("aaa", "")
			b.Flags().Bool("annotate", false, "")
			root.AddCommand(a, b)
		}},
		{"leaf beats parent", "target", "tool target", func(root *cobra.Command) {
			root.AddCommand(runnable("target", ""))
			g := &cobra.Command{Use: "other"}
			p := &cobra.Command{Use: "target"}
			p.AddCommand(runnable("aaa", ""))
			g.AddCommand(p)
			root.AddCommand(g)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := &cobra.Command{Use: "tool"}
			tc.build(root)
			results := clisearch.Search(root, tc.query, 5, clisearch.Options{})
			require.NotEmpty(t, results)
			assert.Equal(t, tc.want, results[0].Command)
		})
	}
}

func TestSearchExclusions(t *testing.T) {
	root := runnable("tool", "needle")
	for _, name := range []string{"help", "completion", "cli", "hidden", "deprecated"} {
		cmd := runnable(name, "needle")
		if name == "hidden" {
			cmd.Hidden = true
		}
		if name == "deprecated" {
			cmd.Deprecated = "use another"
		}
		cmd.AddCommand(runnable("child", "needle"))
		root.AddCommand(cmd)
	}
	root.AddCommand(&cobra.Command{Use: "group", Short: "needle"})
	parent := runnable("skip", "needle")
	parent.AddCommand(runnable("child", "needle"))
	root.AddCommand(parent)
	results := clisearch.Search(root, "needle", 20, clisearch.Options{Skip: func(cmd *cobra.Command) bool { return cmd.Name() == "skip" }})
	assert.Equal(t, []string{"tool skip child"}, paths(results))
}

func TestSearchIgnoredTextAndFlags(t *testing.T) {
	for _, where := range []string{"long", "example", "hidden flag", "deprecated flag", "shadowed inherited", "global flag"} {
		t.Run(where, func(t *testing.T) {
			root := &cobra.Command{Use: "tool"}
			cmd := runnable("show", "")
			root.AddCommand(cmd)
			switch where {
			case "long":
				cmd.Long = "needle"
			case "example":
				cmd.Example = "needle"
			case "hidden flag":
				cmd.Flags().String("needle", "", "")
				require.NoError(t, cmd.Flags().MarkHidden("needle"))
			case "deprecated flag":
				cmd.Flags().String("needle", "", "")
				require.NoError(t, cmd.Flags().MarkDeprecated("needle", "obsolete"))
			case "shadowed inherited":
				root.PersistentFlags().String("same", "", "needle")
				cmd.Flags().String("same", "", "")
				require.NoError(t, cmd.Flags().MarkHidden("same"))
			case "global flag":
				old := pflag.CommandLine
				pflag.CommandLine = pflag.NewFlagSet("global", pflag.ContinueOnError)
				t.Cleanup(func() { pflag.CommandLine = old })
				pflag.CommandLine.String("needle", "", "")
			}
			assert.Empty(t, clisearch.Search(root, "needle", 5, clisearch.Options{}))
		})
	}
}

type observedValue struct{ touched *bool }

func (v observedValue) String() string   { *v.touched = true; return "" }
func (v observedValue) Set(string) error { *v.touched = true; return nil }
func (v observedValue) Type() string     { *v.touched = true; return "observed" }

func TestSearchDoesNotInvokeCallerCode(t *testing.T) {
	root := &cobra.Command{Use: "tool"}
	cmd := runnable("show", "")
	root.AddCommand(cmd)
	touched := false
	cmd.Flags().Var(observedValue{&touched}, "needle", "description")
	root.PersistentFlags().String("inherited", "", "parent")
	cmd.SetGlobalNormalizationFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		touched = true
		return pflag.NormalizedName(name)
	})
	touched = false
	require.Len(t, clisearch.Search(root, "needle", 5, clisearch.Options{}), 1)
	assert.False(t, touched, "search invoked a Value or normalization callback")
	// Search must not merge parent definitions into the child's flag set: a new
	// local shadow remains legal after any number of searches.
	assert.NotPanics(t, func() { cmd.Flags().String("inherited", "", "local") })
}

func TestSearchPrefixThresholdAndStopWords(t *testing.T) {
	root := &cobra.Command{Use: "tool"}
	root.AddCommand(runnable("migrate", ""))
	for _, query := range []string{"mi", "the and to", "...", ""} {
		assert.Empty(t, clisearch.Search(root, query, 5, clisearch.Options{}), query)
	}
	require.Len(t, clisearch.Search(root, "mig", 5, clisearch.Options{}), 1)
}

func TestSearchSynonyms(t *testing.T) {
	root := &cobra.Command{Use: "tool"}
	root.AddCommand(runnable("issue", ""), runnable("delete", ""))
	opts := clisearch.Options{Synonyms: map[string][]string{"TICKET!": {"issues"}, "issue": {"delete"}}}
	assert.Equal(t, []string{"tool issue"}, paths(clisearch.Search(root, "ticket", 5, opts)), "expansion must not be recursive")
	assert.Equal(t, []string{"tool delete"}, paths(clisearch.Search(root, "remove", 5, opts)), "generic synonyms remain")
	assert.Equal(t, []string{"tool issue", "tool delete"}, paths(clisearch.Search(root, "issue", 5, opts)))
	assert.Equal(t, map[string][]string{"TICKET!": {"issues"}, "issue": {"delete"}}, opts.Synonyms)
}

func TestSearchExactBeatsRareExpansion(t *testing.T) {
	for _, tc := range []struct {
		name, query, expansion string
		opts                   clisearch.Options
	}{
		{name: "prefix", query: "run", expansion: "runner"},
		{name: "synonym", query: "show", expansion: "list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := &cobra.Command{Use: "tool"}
			for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
				root.AddCommand(runnable(name, tc.query))
			}
			root.AddCommand(runnable("rare", tc.expansion))
			got := paths(clisearch.Search(root, tc.query, 20, tc.opts))
			require.Len(t, got, 10)
			assert.Equal(t, "tool rare", got[len(got)-1])
		})
	}
}

func TestSearchUsageAndSummary(t *testing.T) {
	for _, use := range []string{"show <ref>", "show <ref> [flags]", "show --name <name> [flags] <ref>"} {
		root := &cobra.Command{Use: "tool"}
		cmd := runnable(use, "Show\n  details\tcompactly")
		cmd.Flags().Bool("all", false, "")
		root.AddCommand(cmd)
		results := clisearch.Search(root, "show", 5, clisearch.Options{})
		require.Len(t, results, 1)
		assert.Equal(t, "tool "+use, results[0].Usage)
		assert.Equal(t, "Show details compactly", results[0].Summary)
	}
	root := &cobra.Command{Use: "tool"}
	cmd := runnable("show <ref>", "Show")
	cmd.Annotations = map[string]string{cobra.CommandDisplayNameAnnotation: "display"}
	root.AddCommand(cmd)
	result := clisearch.Search(root, "show", 5, clisearch.Options{})
	require.Len(t, result, 1)
	assert.Equal(t, "tool display <ref>", result[0].Usage)
	assert.Equal(t, "tool show", result[0].Command)
}

func TestSearchDeterminismAndLimits(t *testing.T) {
	root := &cobra.Command{Use: "tool"}
	for _, name := range []string{"z", "a", "d", "b", "c", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p", "q", "r", "s", "t", "u", "v"} {
		root.AddCommand(runnable(name, "needle"))
	}
	all := clisearch.Search(root, "needle", math.MaxInt, clisearch.Options{})
	require.Len(t, all, 20)
	assert.True(t, slices.IsSorted(paths(all)))
	assert.Equal(t, all, clisearch.Search(root, "needle needle", 20, clisearch.Options{}))
	for _, limit := range []int{math.MinInt, -1, 0, 1, 5, 20, 21, math.MaxInt} {
		got := clisearch.Search(root, "needle", limit, clisearch.Options{})
		assert.NotNil(t, got)
		want := max(0, min(limit, 20))
		assert.Len(t, got, want)
		if want > 0 {
			assert.Equal(t, all[:want], got)
		}
	}
}

func TestKeywordsReplaceAndPreserve(t *testing.T) {
	clisearch.SetKeywords(nil, "ignored")
	cmd := runnable("comment", "")
	cmd.Annotations = map[string]string{"other": "kept"}
	root := &cobra.Command{Use: "tool"}
	root.AddCommand(cmd)
	clisearch.SetKeywords(cmd, "old")
	clisearch.SetKeywords(cmd, "new")
	assert.Empty(t, clisearch.Search(root, "old", 5, clisearch.Options{}))
	require.Len(t, clisearch.Search(root, "new", 5, clisearch.Options{}), 1)
	assert.Equal(t, "kept", cmd.Annotations["other"])
}
