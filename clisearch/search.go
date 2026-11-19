package clisearch

import (
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type field struct {
	counts map[string]int
	length int
}

type document struct {
	result Result
	fields [5]field
}

// Search ranks runnable commands from root's live command tree. It reads only
// metadata: no handlers, lifecycle hooks, help functions or flag Value methods
// are called. Cobra may lazily allocate metadata containers and sort children;
// callers must serialize searches with tree mutation and command execution.
// Flag shadowing uses stored Flag.Name values. Register flags with consistent
// normalization throughout the tree for parser-equivalent shadowing; custom
// normalization callbacks are never evaluated.
//
// The root and non-runnable groups are not results. Hidden and deprecated
// subtrees are excluded, as are NewCommand's group and help and completion
// under the root. Options.Skip excludes only a node. The parent path field
// omits the searched root's command path.
// Nonpositive limits, nil roots and empty queries return a non-nil empty slice;
// limits above 20 are capped at 20. Ties sort by canonical command path.
func Search(root *cobra.Command, query string, limit int, opts Options) []Result {
	results := make([]Result, 0)
	if root == nil || limit <= 0 {
		return results
	}
	groups := expand(query, opts.Synonyms)
	if len(groups) == 0 {
		return results
	}
	docs := documents(root, opts.Skip)
	scores := rank(docs, groups)
	order := make([]int, 0, len(docs))
	for i, score := range scores {
		if score > 0 {
			order = append(order, i)
		}
	}
	slices.SortFunc(order, func(a, b int) int {
		if scores[a] != scores[b] {
			if scores[a] > scores[b] {
				return -1
			}
			return 1
		}
		return strings.Compare(docs[a].result.Command, docs[b].result.Command)
	})
	for _, i := range order[:min(limit, 20, len(order))] {
		results = append(results, docs[i].result)
	}
	return results
}

func documents(root *cobra.Command, skip func(*cobra.Command) bool) []document {
	docs := make([]document, 0)
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.Hidden || cmd.Deprecated != "" {
			return
		}
		if cmd != root {
			if cmd.Annotations[adapterAnnotation] != "" {
				return
			}
			// Cobra adds help and completion only at the root. Deeper commands
			// may reuse those names.
			if cmd.Parent() == root && (cmd.Name() == "help" || cmd.Name() == "completion") {
				return
			}
			if cmd.Runnable() && (skip == nil || !skip(cmd)) {
				docs = append(docs, indexCommand(root, cmd))
			}
		}
		// Commands may sort its internal slice; never change Cobra's global sorting
		// policy. Ranking and tie-breaking do not depend on traversal order.
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
	return docs
}

func indexCommand(root, cmd *cobra.Command) document {
	usage := *cmd
	usage.DisableFlagsInUseLine = true
	doc := document{result: Result{
		Command: cmd.CommandPath(), Summary: strings.Join(strings.Fields(cmd.Short), " "),
		Usage: usage.UseLine(),
	}}
	texts := [5]string{
		cmd.Name() + " " + strings.Join(cmd.Aliases, " "),
		// Every path starts with the root, so naming the tool must not match
		// every command. Index only the path below the searched root.
		strings.TrimPrefix(cmd.Parent().CommandPath(), root.CommandPath()),
		cmd.Annotations[keywordsAnnotation], cmd.Short, flagText(cmd),
	}
	for i, text := range texts {
		words := tokens(text)
		counts := make(map[string]int, len(words))
		for _, word := range words {
			counts[word]++
		}
		doc.fields[i] = field{counts: counts, length: len(words)}
	}
	return doc
}

// flagText enumerates metadata without Cobra's LocalFlags/InheritedFlags:
// those merge definitions, import pflag.CommandLine and invoke normalization
// callbacks. Dedupe before checking visibility so hidden shadows stay hidden.
func flagText(cmd *cobra.Command) string {
	var text strings.Builder
	seen := make(map[string]bool)
	add := func(f *pflag.Flag) {
		if seen[f.Name] {
			return
		}
		seen[f.Name] = true
		if f.Hidden || f.Deprecated != "" {
			return
		}
		text.WriteString(f.Name)
		text.WriteByte(' ')
		text.WriteString(f.Usage)
		text.WriteByte(' ')
	}
	cmd.Flags().VisitAll(add)
	for current := cmd; current != nil; current = current.Parent() {
		current.PersistentFlags().VisitAll(add)
	}
	return text.String()
}

// rank uses BM25 k1=1.2, b=0.75 with field boosts 3/1.5/3/2/1.
// Empty fields contribute zero; average field length uses populated fields only.
// Each original query term shares one pooled DF/IDF across its exact, prefix
// and synonym candidates. Candidate rarity therefore cannot erase discounts.
func rank(docs []document, groups [][]candidate) []float64 {
	scores := make([]float64, len(docs))
	boosts := [5]float64{3, 1.5, 3, 2, 1}
	for f, boost := range boosts {
		total, populated := 0, 0
		for _, doc := range docs {
			if doc.fields[f].length > 0 {
				total += doc.fields[f].length
				populated++
			}
		}
		if total == 0 {
			continue
		}
		average := float64(total) / float64(populated)
		matches := make([]float64, len(docs))
		for _, group := range groups {
			df := 0
			for i, doc := range docs {
				matches[i] = matchField(doc.fields[f], group, average)
				if matches[i] > 0 {
					df++
				}
			}
			if df == 0 {
				continue
			}
			idf := math.Log(1 + (float64(len(docs)-df)+0.5)/(float64(df)+0.5))
			for i, match := range matches {
				scores[i] += boost * idf * match
			}
		}
	}
	return scores
}

func matchField(f field, group []candidate, average float64) float64 {
	best := 0.0
	for _, c := range group {
		for word, count := range f.counts {
			weight := c.weight
			if word != c.text {
				if utf8.RuneCountInString(c.text) < 3 || !strings.HasPrefix(word, c.text) {
					continue
				}
				weight *= 0.5
			}
			tf := float64(count)
			saturated := tf * 2.2 / (tf + 1.2*(0.25+0.75*float64(f.length)/average))
			best = max(best, weight*saturated)
		}
	}
	return best
}
