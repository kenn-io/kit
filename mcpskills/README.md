---
last_edited: 2026-11-05
---

# Bundled MCP guides

`mcpskills` lets an MCP server teach clients how to use its tools from the
authored Markdown the application already bundles. It serves a fixed catalog;
it does not load paths or execute skills.

```go
catalog, err := mcpskills.New(mcpskills.Config{
    ToolName: "example.skills",
    Guides: []mcpskills.Guide{{
        Entry: mcpskills.Entry{
            Name: "workflow", Title: "Workflow", Summary: "Start here",
        },
        Markdown: bundledWorkflow,
    }},
})
if err != nil {
    return err
}
server := mcp.NewServer(implementation, &mcp.ServerOptions{
    Instructions: existingInstructions + "\n\n" + catalog.Instructions(),
})
catalog.Register(server)
```

Use string constants or `go:embed` for `bundledWorkflow`. If the CLI already
ships the guide, reuse that source. Render any existing application templates at
startup, then append accurate command-to-tool mappings and MCP differences in
the application. Kit does not interpret those mappings or templates.

Call `example.skills {}` to list `{name, title, summary}` entries in authored
order. Call `example.skills {"name":"workflow"}` to retrieve exact Markdown as
text content. Both calls return a structured object with a `skills` array;
retrieval contains one entry. The structured result never repeats the Markdown.
`Output` provides a type for consumers decoding that metadata.

Names are case-sensitive ASCII identifiers matching `[a-z][a-z0-9_-]*`.
The optional `name` must be a nonempty string when present. Null, padded names,
unknown fields, repeated fields, malformed input and unknown names return
JSON-RPC invalid-params (`-32602`). Omitted arguments and `{}` list the catalog.
Construction rejects duplicate names, empty catalogs, blank text and invalid
UTF-8. Accepted titles, summaries and Markdown retain their exact bytes.

Applications with central tool catalogs can use `catalog.Tool()` and
`catalog.Handler` independently with the SDK's `server.AddTool`. The raw handler
validates its own input and supplies both text and structured content. Each call
and tool definition owns its mutable result; callers can adapt a returned schema
or tool annotation without changing the catalog.

Append `catalog.Instructions()` before constructing the server; it preserves
caller control over existing instructions. Register the guide tool outside
optional sections to keep it always discoverable. Applications own their
catalog filters, deferred loading, authentication, authorization, result bounds
and transport. The helper does not bypass that policy or turn guide text into
an action. See the runnable two-consumer example and in-memory protocol tests.
