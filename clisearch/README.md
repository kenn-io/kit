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
and accepts 1–20. No matches is a successful empty result with a root-help hint;
a blank task is a usage error. `cli` alone prints its help.

## Output and lifecycle integration

The caller owns JSON selection and error conventions. The adapter adds only
`--limit`, preserves inherited command hooks, and writes text to Cobra's output
writer. Exempt it from application configuration, authentication, workspace or
daemon initialization using the application's existing mechanism. Cobra global
initializers run before argument validation and remain the caller's concern.

Use `Render` to select the existing JSON writer, retaining `RenderText` for
text. `UsageError` can wrap validation and flag-parse errors in the application's
usage-error type; render errors propagate unchanged.

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

When `UsageError` is nil, the adapter preserves the inherited `FlagErrorFunc`.
If flag parsing needs a classifier distinct from argument validation, replace
the returned group's and search child's `FlagErrorFunc` with delegates to the
root handler. The returned Cobra commands are ordinary mutable commands; the
adapter installs no custom pre-run hooks.

The JSON-ready response has no scores. `Results` is always non-nil, so no hits
encode as `[]`:

```json
{"query":"add a comment","results":[{"command":"tool comment","summary":"Add a comment","usage":"tool comment <ref> -m <text>"}]}
```

`Query` preserves the joined arguments, including outer whitespace. `Command`
is the canonical path. `Summary` flattens whitespace in `Short`; `Usage` keeps
Cobra's authored `Use` syntax and display name without appending automatic
`[flags]`. An explicit `[flags]` in `Use` remains intact.

## Ranking and metadata

Search indexes these fields with separate BM25-style length normalization:

| Field | Weight |
|---|---:|
| Leaf name and aliases | 3 |
| Parent command path | 1.5 |
| Keywords | 3 |
| `Short` | 2 |
| Visible flag names and descriptions, including inherited flags | 1 |

`Long` and `Example` are excluded. Normalization lowercases Unicode words,
splits punctuation, removes a short English stop-word list, and applies light
English plural/past/gerund folding. This is keyword search, not semantic search.
Prefix matches require at least three normalized characters and receive half
weight. Synonyms receive 0.6 weight; synonym prefixes receive 0.3.

Generic synonym groups cover remove/delete/rm, list/show/ls, create/add/new,
todo/task, find/search/lookup and config/settings. Add directed domain query
expansions without changing the generic groups:

```go
opts.Synonyms = map[string][]string{"ticket": {"issue"}}
clisearch.SetKeywords(commentCmd, "log a note", "annotate")
```

Keyword calls replace the prior keyword list and preserve other annotations.
Only add metadata where task examples reveal a missed command. Expansion is
not recursive and never mutates the supplied synonym map. Multiple candidate
matches score once per original normalized query term in each field.

Ranking uses BM25 `k1=1.2`, `b=0.75`, with average length over populated fields.
A sparse keyword field therefore keeps its intended weight. Document frequency
is pooled across the exact/prefix/synonym candidates for one original term,
so rare expansions cannot defeat their discounts on equivalent field evidence.
Ties sort by command path; repeated normalized query words do not add weight.

## Eligibility and the pure API

```go
results := clisearch.Search(root, task, 5, opts)
```

The root, non-runnable groups, hidden/deprecated commands, and conventional
`help`, `completion` and `cli` subtrees do not appear. Hidden/deprecated parents
exclude their descendants. A caller's `Skip` predicate excludes only that node;
children are still considered. This supports applications with runnable
help-only parents. If an entire family is unusable, skip each descendant too.
Do not probe live service availability from `Skip`.

The pure API returns an empty non-nil slice for nil roots, empty or
stop-word-only queries and nonpositive limits; limits above 20 clamp to 20.
Search never calls handlers, custom help, flag values or flag normalization
callbacks, and never imports the process's global flags. It may allocate
Cobra's lazy flag containers and sort child lists. Serialize searches with
other searches, command execution and tree mutation; Cobra trees are mutable.

Flag shadowing uses stored `Flag.Name` values, with the nearest definition
winning even when hidden or deprecated. Register flags with consistent
normalization across the tree for parser-equivalent shadowing (for example,
install a root global normalization function before registration). Search does
not evaluate heterogeneous command-specific normalizers.
