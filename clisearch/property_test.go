package clisearch_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/clisearch"
)

// checkProperty runs f on n generated inputs. f reports failures through
// testify and returns false to stop at the first counterexample.
func checkProperty(t *testing.T, n int, f any) {
	t.Helper()
	require.NoError(t, quick.Check(f, &quick.Config{MaxCount: n}))
}

// propertyQuery is arbitrary text, a sampled example or raw bytes. Samples
// include blank text, which uniform generation almost never produces.
type propertyQuery string

func (propertyQuery) Generate(r *rand.Rand, size int) reflect.Value {
	if r.Intn(2) == 0 {
		raw := make([]byte, r.Intn(size+1))
		r.Read(raw)
		return reflect.ValueOf(propertyQuery(raw))
	}
	if r.Intn(2) == 0 {
		samples := []string{"needle", "remove", "TICKET!", "migrat", "comment", "the and", "", " \t"}
		return reflect.ValueOf(propertyQuery(samples[r.Intn(len(samples))]))
	}
	text, _ := quick.Value(reflect.TypeFor[string](), r)
	return reflect.ValueOf(propertyQuery(text.String()))
}

// propertyLimit covers the full int domain but favors the accepted 1–20 range
// and its boundaries. Uniform ints never land there.
type propertyLimit int

func (propertyLimit) Generate(r *rand.Rand, _ int) reflect.Value {
	var limit int
	switch r.Intn(4) {
	case 0:
		limit = []int{math.MinInt, -1, 0, 1, 19, 20, 21, math.MaxInt}[r.Intn(8)]
	case 1:
		limit = int(r.Uint64())
	default:
		limit = r.Intn(30) - 5
	}
	return reflect.ValueOf(propertyLimit(limit))
}

// propertyLetters is a non-empty run of non-ASCII Unicode letters.
type propertyLetters []string

func propertyLetter(r *rand.Rand) string {
	for {
		if c := rune(128 + r.Intn(unicode.MaxRune-127)); unicode.IsLetter(c) {
			return string(c)
		}
	}
}

func (propertyLetters) Generate(r *rand.Rand, size int) reflect.Value {
	letters := make(propertyLetters, 1+r.Intn(size))
	for i := range letters {
		letters[i] = propertyLetter(r)
	}
	return reflect.ValueOf(letters)
}

func propertyTree() *cobra.Command {
	root := &cobra.Command{Use: "tool"}
	for i := range 30 {
		cmd := runnable(fmt.Sprintf("item%02d", i), "needle comment details")
		if i%3 == 0 {
			cmd.Aliases = []string{"remove"}
		}
		if i%5 == 0 {
			clisearch.SetKeywords(cmd, "issue", "migrate")
		}
		cmd.Flags().Bool("all", false, "include every record")
		root.AddCommand(cmd)
	}
	return root
}

func TestSearchLimitProperty(t *testing.T) {
	root := propertyTree()
	checkProperty(t, 2000, func(query propertyQuery, limit propertyLimit) bool {
		all := clisearch.Search(root, string(query), 20, clisearch.Options{})
		got := clisearch.Search(root, string(query), int(limit), clisearch.Options{})
		expected := all[:min(max(int(limit), 0), len(all))]
		return assert.Equal(t, expected, got) && assert.NotNil(t, got)
	})
}

func TestSearchDeterminismProperty(t *testing.T) {
	root := propertyTree()
	checkProperty(t, 2000, func(query propertyQuery, limit propertyLimit) bool {
		opts := clisearch.Options{Synonyms: map[string][]string{"TICKET": {"issue"}, "ticket!": {"comment"}}}
		first := clisearch.Search(root, string(query), int(limit), opts)
		second := clisearch.Search(root, string(query), int(limit), opts)
		return assert.Equal(t, first, second)
	})
}

func TestSearchEligibilityProperty(t *testing.T) {
	// Per command: hidden, deprecated, skipped.
	checkProperty(t, 2000, func(draws [5][3]bool) bool {
		root := &cobra.Command{Use: "tool", Run: func(*cobra.Command, []string) {}}
		allowed := make(map[string]bool)
		skipped := make(map[*cobra.Command]bool)
		for i, name := range []string{"one", "two", "help", "completion", "cli"} {
			parent := runnable(name, "needle")
			parent.Hidden = draws[i][0]
			deprecated := draws[i][1]
			if deprecated {
				parent.Deprecated = "obsolete"
			}
			skipped[parent] = draws[i][2]
			child := runnable("child", "needle")
			parent.AddCommand(child)
			root.AddCommand(parent)
			excluded := parent.Hidden || deprecated || name == "help" || name == "completion" || name == "cli"
			if !excluded {
				if !skipped[parent] {
					allowed["tool "+name] = true
				}
				allowed["tool "+name+" child"] = true
			}
		}
		results := clisearch.Search(root, "needle", 20, clisearch.Options{Skip: func(cmd *cobra.Command) bool { return skipped[cmd] }})
		got := make(map[string]bool)
		for _, result := range results {
			got[result.Command] = true
		}
		return assert.Equal(t, allowed, got)
	})
}

func TestSearchSynonymOrderProperty(t *testing.T) {
	root := propertyTree()
	checkProperty(t, 2000, func(value string) bool {
		a := clisearch.Options{Synonyms: map[string][]string{"TICKET": {"issue", value}, "ticket!": {"comment"}}}
		b := clisearch.Options{Synonyms: map[string][]string{"ticket!": {"comment", "comment"}, "TICKET": {value, "issue", value}}}
		return assert.Equal(t, clisearch.Search(root, "ticket", 20, a), clisearch.Search(root, "ticket", 20, b))
	})
}

func TestCommandSearchAgreementProperty(t *testing.T) {
	checkProperty(t, 2000, func(generated propertyQuery, generatedLimit propertyLimit) bool {
		query, limit := string(generated), int(generatedLimit)
		root := commandTree()
		root.SilenceErrors = true
		root.SilenceUsage = true
		// Enough matches for every accepted limit to truncate the results.
		for i := range 25 {
			root.AddCommand(runnable(fmt.Sprintf("item%02d", i), "needle comment details"))
		}
		var got clisearch.Response
		sentinel := errors.New("usage")
		opts := clisearch.Options{
			UsageError: func(err error) error { return fmt.Errorf("%w: %w", sentinel, err) },
			Render:     func(_ *cobra.Command, response clisearch.Response) error { got = response; return nil },
		}
		root.AddCommand(clisearch.NewCommand(root, opts))
		root.SetArgs([]string{"cli", "search", fmt.Sprintf("--limit=%d", limit), "--", query})
		err := root.ExecuteContext(t.Context())
		if strings.TrimSpace(query) == "" || limit < 1 || limit > 20 {
			return assert.ErrorIs(t, err, sentinel)
		}
		return assert.NoError(t, err) &&
			assert.Equal(t, clisearch.Response{Query: query, Results: clisearch.Search(root, query, limit, opts)}, got)
	})
}

func BenchmarkSearch(b *testing.B) {
	root := &cobra.Command{Use: "tool"}
	for i := range 500 {
		cmd := runnable(fmt.Sprintf("record%03d", i), "Search archived records by author and date")
		cmd.Flags().String("author", "", "filter by author name")
		cmd.Flags().String("since", "", "include records after date")
		if i%10 == 0 {
			clisearch.SetKeywords(cmd, "find history lookup")
		}
		root.AddCommand(cmd)
	}
	b.ReportAllocs()
	for b.Loop() {
		clisearch.Search(root, "find records by author", 5, clisearch.Options{})
	}
}

func TestTypicalTextSize(t *testing.T) {
	root := propertyTree()
	var output bytes.Buffer
	root.SetOut(&output)
	require.NoError(t, clisearch.RenderText(root, clisearch.Response{Query: "comment", Results: clisearch.Search(root, "comment", 5, clisearch.Options{})}))
	assert.Less(t, output.Len(), 1024)
	t.Logf("five results and hint: %d bytes", output.Len())
}

func TestSearchSparseFieldProperty(t *testing.T) {
	checkProperty(t, 1000, func(n uint16) bool {
		count := int(n % 501)
		root := &cobra.Command{Use: "tool"}
		keyword := runnable("target", "")
		clisearch.SetKeywords(keyword, "needle")
		root.AddCommand(keyword, runnable("aaa", "needle"))
		for i := range count {
			root.AddCommand(runnable(fmt.Sprintf("extra%d", i), "unrelated"))
		}
		got := clisearch.Search(root, "needle", 20, clisearch.Options{})
		return assert.Len(t, got, 2) && assert.Equal(t, "tool target", got[0].Command)
	})
}

func TestSearchCandidatePriorityProperty(t *testing.T) {
	checkProperty(t, 1000, func(n uint8) bool {
		count := int(n % 101)
		root := &cobra.Command{Use: "tool"}
		root.AddCommand(runnable("exact", "quartz"), runnable("synonym", "zephyr"), runnable("prefix", "quartzen"), runnable("synprefix", "zephyren"))
		for i := range count {
			root.AddCommand(runnable(fmt.Sprintf("other%d", i), "quartz"))
		}
		got := clisearch.Search(root, "quartz", 20, clisearch.Options{Synonyms: map[string][]string{"quartz": {"zephyr"}}})
		// Exact evidence must remain ahead even when expansions are rarer.
		return assert.NotEmpty(t, got) && assert.Equal(t, "tool exact", got[0].Command)
	})
}

func TestSearchArbitraryBytesKnownHitProperty(t *testing.T) {
	root := &cobra.Command{Use: "tool"}
	root.AddCommand(runnable("target", "quartz"))
	checkProperty(t, 2000, func(raw []byte) bool {
		got := clisearch.Search(root, string(raw)+" quartz", 5, clisearch.Options{})
		return assert.Len(t, got, 1) && assert.Equal(t, "tool target", got[0].Command)
	})
}

func TestSearchNormalizedFlagShadowProperty(t *testing.T) {
	checkProperty(t, 1000, func(depth uint8, hidden, warm bool) bool {
		root := &cobra.Command{Use: "tool"}
		armed := false
		root.SetGlobalNormalizationFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
			if armed {
				panic("normalizer called")
			}
			return pflag.NormalizedName(strings.ReplaceAll(name, "_", "-"))
		})
		root.PersistentFlags().String("output_format", "", "ancestorneedle")
		group := &cobra.Command{Use: "group"}
		root.AddCommand(group)
		leaf := runnable("leaf", "")
		group.AddCommand(leaf)
		sets := []*pflag.FlagSet{leaf.Flags(), leaf.PersistentFlags(), group.PersistentFlags()}
		set := sets[depth%3]
		set.String("output_format", "", "localneedle")
		if hidden {
			require.NoError(t, set.MarkHidden("output_format"))
		} else {
			require.NoError(t, set.MarkDeprecated("output_format", "obsolete"))
		}
		if warm {
			require.NoError(t, leaf.ParseFlags(nil))
		}
		// Arm the trap after fixture construction and optional Cobra warmup.
		armed = true
		return assert.Empty(t, clisearch.Search(root, "ancestorneedle", 20, clisearch.Options{}))
	})
}

func TestSearchAlternativesDoNotAccumulateProperty(t *testing.T) {
	checkProperty(t, 2000, func(seed uint, n uint8) bool {
		count := 2 + int(n%4)
		query := fmt.Sprintf("target%d", seed)
		alternatives := make([]string, count)
		for i := range alternatives {
			alternatives[i] = fmt.Sprintf("candidate%d%d", seed, i)
		}
		root := &cobra.Command{Use: "tool"}
		root.AddCommand(
			runnable("exact", query),
			runnable("single", alternatives[0]),
			runnable("multiple", strings.Join(alternatives, " ")),
		)
		got := clisearch.Search(root, query, 20, clisearch.Options{Synonyms: map[string][]string{query: alternatives}})
		return assert.Equal(t, []string{"tool exact", "tool single", "tool multiple"}, paths(got))
	})
}

func TestSearchInsertionOrderProperty(t *testing.T) {
	// Exercise traversal order instead of letting Cobra canonicalize both trees.
	sorting := cobra.EnableCommandSorting
	cobra.EnableCommandSorting = false
	t.Cleanup(func() { cobra.EnableCommandSorting = sorting })
	checkProperty(t, 2000, func(n uint8, seed uint, permutation int64) bool {
		count := 4 + int(n%37)
		term := fmt.Sprintf("needle%d", seed)
		summaries := make([]string, count)
		for i := range summaries {
			// Pairs have identical evidence (ties); other pairs have different scores.
			summaries[i] = strings.Repeat(term+" ", 1+i/2) + "unrelated"
		}
		build := func(order []int) *cobra.Command {
			root := &cobra.Command{Use: "tool"}
			group := &cobra.Command{Use: "group"}
			root.AddCommand(group)
			for _, i := range order {
				group.AddCommand(runnable(fmt.Sprintf("item%02d", i), summaries[i]))
			}
			return root
		}
		r := rand.New(rand.NewSource(permutation))
		a := clisearch.Search(build(r.Perm(count)), term, 20, clisearch.Options{})
		b := clisearch.Search(build(r.Perm(count)), term, 20, clisearch.Options{})
		return assert.Len(t, a, min(count, 20)) && assert.Equal(t, a, b)
	})
}

func TestSearchQueryOrderDoesNotChangeRankingProperty(t *testing.T) {
	checkProperty(t, 2000, func(seed uint, n, repeat uint8, permutation int64) bool {
		count := 2 + int(n%4)
		terms := make([]string, count)
		for i := range terms {
			terms[i] = fmt.Sprintf("term%dq%d", seed, i)
		}
		root := &cobra.Command{Use: "tool"}
		root.AddCommand(runnable("both", strings.Join(terms, " ")))
		for i, term := range terms {
			root.AddCommand(runnable(fmt.Sprintf("entry%d", i), term))
		}
		forward := clisearch.Search(root, strings.Join(terms, " "), 20, clisearch.Options{})
		// Different punctuation/case still denotes the same normalized terms.
		reordered := append(slices.Clone(terms), terms[int(repeat)%count])
		slices.Reverse(reordered)
		reverse := clisearch.Search(root, strings.ToUpper(strings.Join(reordered, "! ")), 20, clisearch.Options{})
		permuted := make([]string, len(reordered))
		for i, j := range rand.New(rand.NewSource(permutation)).Perm(len(reordered)) {
			permuted[i] = reordered[j]
		}
		shuffled := clisearch.Search(root, strings.Join(permuted, " "), 20, clisearch.Options{})
		return assert.Len(t, forward, len(terms)+1) && assert.Equal(t, "tool both", forward[0].Command) &&
			assert.Equal(t, forward, reverse) && assert.Equal(t, forward, shuffled)
	})
}

func TestSearchRepeatedQueryTermsDoNotChangeRankingProperty(t *testing.T) {
	checkProperty(t, 2000, func(seed uint, n uint8) bool {
		repeats := 2 + int(n%4)
		first := fmt.Sprintf("first%d", seed)
		second := fmt.Sprintf("second%d", seed)
		root := &cobra.Command{Use: "tool"}
		root.AddCommand(runnable("single", first), runnable("repeated", second+" "+second))

		got := clisearch.Search(root, first+" "+second, 5, clisearch.Options{})
		repeated := clisearch.Search(root, strings.Repeat(first+" ", repeats)+second, 5, clisearch.Options{})
		return assert.Len(t, got, 2) && assert.Equal(t, "tool repeated", got[0].Command) && assert.Equal(t, got, repeated)
	})
}

func TestSearchUnicodePrefixProperty(t *testing.T) {
	checkProperty(t, 2000, func(letters propertyLetters, seed int64) bool {
		prefix := strings.Join(letters, "")
		word := prefix + propertyLetter(rand.New(rand.NewSource(seed)))
		root := &cobra.Command{Use: "tool"}
		root.AddCommand(runnable("one", word))
		got := clisearch.Search(root, prefix, 20, clisearch.Options{})
		if utf8.RuneCountInString(prefix) < 3 {
			return assert.Empty(t, got)
		}
		return assert.Equal(t, []clisearch.Result{{Command: "tool one", Summary: word, Usage: "tool one"}}, got)
	})
}

func TestSearchLiveMetadataProperty(t *testing.T) {
	checkProperty(t, 1000, func(text string) bool {
		root := &cobra.Command{Use: "tool"}
		leaf := runnable("one", text)
		root.AddCommand(leaf)
		clisearch.Search(root, "quartz", 20, clisearch.Options{})
		leaf.Short = "quartz " + text
		fresh := &cobra.Command{Use: "tool"}
		fresh.AddCommand(runnable("one", leaf.Short))
		got := clisearch.Search(root, "quartz", 20, clisearch.Options{})
		return assert.Len(t, got, 1) && assert.Equal(t, clisearch.Search(fresh, "quartz", 20, clisearch.Options{}), got)
	})
}

func TestSearchAuthoredUsageProperty(t *testing.T) {
	checkProperty(t, 1000, func(suffix, display string, explicit, disabled bool) bool {
		root := &cobra.Command{Use: "tool"}
		group := &cobra.Command{Use: "group"}
		root.AddCommand(group)
		leaf := runnable("one "+suffix, "quartz")
		group.AddCommand(leaf)
		leaf.Annotations = map[string]string{cobra.CommandDisplayNameAnnotation: display}
		leaf.Flags().Bool("verbose", false, "extra output")
		leaf.DisableFlagsInUseLine = disabled
		if explicit {
			leaf.Use += " [flags]"
		}
		// Disable automatic flags so Cobra's independent UseLine oracle preserves
		// all authored syntax, even when the generated suffix already has [flags].
		oracle := *leaf
		oracle.DisableFlagsInUseLine = true
		got := clisearch.Search(root, "quartz", 20, clisearch.Options{})
		return assert.Len(t, got, 1) && assert.Equal(t, oracle.UseLine(), got[0].Usage) &&
			assert.Equal(t, disabled, leaf.DisableFlagsInUseLine)
	})
}

type failAtWriter struct {
	remaining int
	err       error
}

func (w *failAtWriter) Write(p []byte) (int, error) {
	if w.remaining == 0 {
		return 0, w.err
	}
	w.remaining--
	return len(p), nil
}

func TestRenderTextErrorIdentityProperty(t *testing.T) {
	checkProperty(t, 1000, func(n, failAt uint8) bool {
		count := int(n % 21)
		at := int(failAt) % (count + 1)
		root := &cobra.Command{Use: "tool"}
		sentinel := errors.New("writer failure")
		root.SetOut(&failAtWriter{remaining: at, err: sentinel})
		response := clisearch.Response{Results: make([]clisearch.Result, count)}
		for i := range response.Results {
			response.Results[i] = clisearch.Result{Command: fmt.Sprintf("tool item%d", i), Summary: "quartz"}
		}
		return assert.ErrorIs(t, clisearch.RenderText(root, response), sentinel)
	})
}
