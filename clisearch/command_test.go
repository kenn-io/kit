package clisearch_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/clisearch"
)

func commandTree() *cobra.Command {
	root := &cobra.Command{Use: "tool", SilenceErrors: true, SilenceUsage: true}
	root.AddCommand(&cobra.Command{Use: "comment <ref>", Short: "Add a comment", Run: func(*cobra.Command, []string) {}})
	return root
}

func execute(t *testing.T, opts clisearch.Options, args ...string) (string, error) {
	t.Helper()
	root := commandTree()
	root.AddCommand(clisearch.NewCommand(root, opts))
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(args)
	err := root.ExecuteContext(t.Context())
	return output.String(), err
}

func TestCommandDefaultText(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	output, err := execute(t, clisearch.Options{}, "cli", "search", "add", "comment")
	require.NoError(t, err)
	assert.Equal(t, "tool comment  Add a comment\nRun 'tool comment --help' for flags and examples.\n", output)
}

func TestCommandJSONCallback(t *testing.T) {
	var got clisearch.Response
	output, err := execute(t, clisearch.Options{Render: func(cmd *cobra.Command, response clisearch.Response) error {
		got = response
		return json.NewEncoder(cmd.OutOrStdout()).Encode(response)
	}}, "cli", "search", " add", "comment ")
	require.NoError(t, err)
	assert.Equal(t, " add comment ", got.Query)
	require.Len(t, got.Results, 1)
	assert.JSONEq(t, `{"query":" add comment ","results":[{"command":"tool comment","summary":"Add a comment","usage":"tool comment <ref>"}]}`, output)
}

func TestCommandEmptyResults(t *testing.T) {
	output, err := execute(t, clisearch.Options{}, "cli", "search", "zxqv")
	require.NoError(t, err)
	assert.Equal(t, "Run 'tool --help' for flags and examples.\n", output)
	output, err = execute(t, clisearch.Options{Render: func(cmd *cobra.Command, response clisearch.Response) error {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(response)
	}}, "cli", "search", "zxqv")
	require.NoError(t, err)
	assert.JSONEq(t, `{"query":"zxqv","results":[]}`, output)
}

func TestCommandUsageErrors(t *testing.T) {
	sentinel := errors.New("native usage error")
	for _, args := range [][]string{
		{"cli", "search"},
		{"cli", "search", "   "},
		{"cli", "search", "x", "--limit=0"},
		{"cli", "search", "x", "--limit=21"},
		{"cli", "search", "x", "--limit=nope"},
		{"cli", "search", "x", "--limit"},
		{"cli", "search", "x", "--unknown"},
		{"cli", "extra"},
		{"cli", "--unknown"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			_, err := execute(t, clisearch.Options{UsageError: func(err error) error {
				return fmt.Errorf("%w: %w", sentinel, err)
			}}, args...)
			assert.ErrorIs(t, err, sentinel)
		})
	}
}

func TestCommandLimit(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"cli", "search", "find"}, 5},
		{[]string{"cli", "search", "find", "--limit=1"}, 1},
		{[]string{"cli", "search", "find", "--limit=20"}, 20},
	} {
		t.Run(fmt.Sprint(tc.args), func(t *testing.T) {
			root := commandTree()
			for i := range 25 {
				root.AddCommand(&cobra.Command{Use: fmt.Sprintf("find%02d", i), Short: "find", Run: func(*cobra.Command, []string) {}})
			}
			var got clisearch.Response
			root.AddCommand(clisearch.NewCommand(root, clisearch.Options{Render: func(_ *cobra.Command, response clisearch.Response) error { got = response; return nil }}))
			root.SetArgs(tc.args)
			require.NoError(t, root.ExecuteContext(t.Context()))
			assert.Len(t, got.Results, tc.want)
		})
	}
}

func TestCommandPreservesHooksAndFlagErrors(t *testing.T) {
	root := commandTree()
	runs := 0
	root.PersistentPreRunE = func(*cobra.Command, []string) error { runs++; return nil }
	sentinel := errors.New("inherited flag error")
	root.SetFlagErrorFunc(func(*cobra.Command, error) error { return sentinel })
	root.AddCommand(clisearch.NewCommand(root, clisearch.Options{Render: func(*cobra.Command, clisearch.Response) error { return nil }}))
	root.SetArgs([]string{"cli", "search", "comment"})
	require.NoError(t, root.ExecuteContext(t.Context()))
	assert.Equal(t, 1, runs)
	root.SetArgs([]string{"cli", "search", "--limit=nope", "comment"})
	require.ErrorIs(t, root.ExecuteContext(t.Context()), sentinel)
	assert.Equal(t, 1, runs)
}

func TestCommandInvalidArgsPrecedeCommandHooks(t *testing.T) {
	root := commandTree()
	root.PersistentPreRunE = func(*cobra.Command, []string) error { panic("invalid arguments reached command pre-run") }
	root.AddCommand(clisearch.NewCommand(root, clisearch.Options{}))
	root.SetArgs([]string{"cli", "search", "--limit=0", "comment"})
	assert.Error(t, root.ExecuteContext(t.Context()))
}

func TestCommandGroupHelp(t *testing.T) {
	output, err := execute(t, clisearch.Options{}, "cli")
	require.NoError(t, err)
	assert.Contains(t, output, "search")
	assert.Contains(t, output, "Available Commands:")
}

func TestCommandRendererError(t *testing.T) {
	sentinel := errors.New("write failed")
	_, err := execute(t, clisearch.Options{
		Render:     func(*cobra.Command, clisearch.Response) error { return sentinel },
		UsageError: func(error) error { panic("render error was reclassified") },
	}, "cli", "search", "comment")
	assert.ErrorIs(t, err, sentinel)
}

func TestSearchBasicAndLive(t *testing.T) {
	root := commandTree()
	results := clisearch.Search(root, "comment", 5, clisearch.Options{})
	require.Len(t, results, 1)
	assert.Equal(t, clisearch.Result{Command: "tool comment", Summary: "Add a comment", Usage: "tool comment <ref>"}, results[0])
	root.AddCommand(&cobra.Command{Use: "review", Short: "Review changes", Run: func(*cobra.Command, []string) {}})
	require.Len(t, clisearch.Search(root, "review", 5, clisearch.Options{}), 1)
	assert.NotNil(t, clisearch.Search(nil, "x", 5, clisearch.Options{}))
}
