package clisearch_test

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/clisearch"
)

func ExampleSearch() {
	root := &cobra.Command{Use: "tool"}
	comment := &cobra.Command{Use: "comment <ref> -m <text>", Short: "Add a comment", Run: func(*cobra.Command, []string) {}}
	clisearch.SetKeywords(comment, "log a note")
	root.AddCommand(comment)
	root.AddCommand(clisearch.NewCommand(root, clisearch.Options{}))
	for _, result := range clisearch.Search(root, "add a comment", 5, clisearch.Options{}) {
		fmt.Printf("%s  %s\n", result.Command, result.Summary)
	}
	// Output:
	// tool comment  Add a comment
}
