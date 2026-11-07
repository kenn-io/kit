# Application identity for MCP

`mcpidentity` snapshots caller-supplied public metadata and a PNG. It produces
an SDK `Implementation` with a website and embedded PNG data URI, usable by
stdio and HTTP clients without fetching an image from another server. It has
no built-in branding, credentials, filesystem access or external requests.

```go
// iconPNG is an application-owned asset, for example from go:embed.
identity, err := mcpidentity.New(mcp.Implementation{
    Name: "example-app",
    Version: version,
    WebsiteURL: "https://example.com",
}, iconPNG)
if err != nil {
    return err
}

// Use this same metadata for the existing stdio server and fresh HTTP servers.
newServer := func() *mcp.Server {
    server := mcp.NewServer(identity.Implementation(), nil)
    // Register application tools here.
    return server
}
endpoint, err := mcphttp.New(func(*http.Request) *mcp.Server {
    return newServer()
}, mcphttp.Options{Authenticate: authenticate})
if err != nil {
    return err
}
mux.Handle("/mcp", endpoint)
mux.Handle("/icon.png", identity.IconHandler())
mux.Handle("/favicon.ico", identity.IconHandler())
```

Imports are `net/http`, `github.com/modelcontextprotocol/go-sdk/mcp`,
`go.kenn.io/kit/mcpidentity` and `go.kenn.io/kit/mcphttp`. The application still
owns stdio startup, tool registration, authentication and endpoint shutdown.
This package also works directly with the SDK, without `mcphttp`.

The SDK advertises the same metadata through legacy initialization and modern
per-result `_meta.serverInfo` where supported. Kit does not emulate old SDKs
or rewrite protocol replies. `Implementation` returns an independent value
for each server; HTTP request identity and authorization remain separate from
this public application identity.

Opt in to the image handler by mounting its two exact paths. Both serve the
same bytes as the data URI, including `/favicon.ico`, whose media type is
`image/png`. GET returns the image; HEAD returns identical headers without a
body. Responses set `nosniff`, a content length and `public, max-age=3600`.
Other methods receive 405 and other paths receive 404. The assets may be
cached for an hour after updates. Host/TLS policy remains the application's.

Only these image routes are public. Keep `/mcp` wrapped in the application's
authentication middleware. If the application instead applies global bearer
authentication, explicitly exempt just these image routes or leave them
private; MCP data-URI icons still require no image request.

The constructor validates PNG header/dimensions and snapshots the exact bytes;
it does not transcode or fully decode pixels. Use a trusted application asset,
not user-provided images. Calling it with invalid or absent PNG headers returns
an error. The supplied icon replaces any `Implementation.Icons`; other fields
are preserved. All identity fields and assets are intended to be public.
