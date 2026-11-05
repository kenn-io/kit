# clisearch

Find runnable Cobra commands by describing a task. Search reads the live command
metadata locally; it does not execute handlers, open configuration, connect to
a daemon, or call a model.

```go
root.AddCommand(clisearch.NewCommand(root, clisearch.Options{}))
```

The returned `cli` group contains `search <task words...>`. Quoting is optional:

```text
tool cli search add a comment
tool cli search "add a comment" --limit 3
```

Text output lists `command  summary` and one help hint. `--limit` defaults to 5
and accepts 1–20. No matches is a successful empty result; a blank task is a
usage error.

## Integrating with an application

The adapter adds only `--limit` and keeps inherited command hooks. Exempt
`cli search` from configuration, authentication or daemon startup with the
application's existing mechanism. Use `Render` for the application's JSON
selector and `UsageError` for its usage-error type:

```go
opts := clisearch.Options{
    Render: func(cmd *cobra.Command, response clisearch.Response) error {
        if jsonOutput { // the application's existing selector
            return json.NewEncoder(cmd.OutOrStdout()).Encode(response)
        }
        return clisearch.RenderText(cmd, response)
    },
    UsageError: func(err error) error { return usageError(err) },
}
root.AddCommand(clisearch.NewCommand(root, opts))
```

The response has no scores, and `Results` is never nil:

```json
{"query":"add a comment","results":[{"command":"tool comment","summary":"Add a comment","usage":"tool comment <ref> -m <text>"}]}
```

Call `clisearch.Search(root, task, limit, opts)` directly to skip the adapter.

## What search matches

| Field | Weight |
|---|---:|
| Leaf name and aliases | 3 |
| Parent command path | 1.5 |
| Keywords | 3 |
| `Short` | 2 |
| Visible flag names and descriptions, including inherited flags | 1 |

This is keyword search with light English plural and tense folding, prefix
matches of three or more characters, and generic synonyms such as
remove/delete and list/show. Add domain words where task examples miss a
command:

```go
opts.Synonyms = map[string][]string{"ticket": {"issue"}}
clisearch.SetKeywords(commentCmd, "log a note", "annotate")
```

Hidden and deprecated commands, their descendants, and the root-level `help`,
`completion` and `cli` commands never appear. `Options.Skip` excludes one
command but still searches its children. Cobra trees are mutable, so do not run
searches concurrently with each other, command execution or tree changes.
