# MCP on the application's HTTP server

`mcphttp` mounts native Streamable HTTP at `/mcp` in an existing Go service.
The same binary and container serve the API and MCP; no stdio proxy or second
listener is needed. `agentmcp` is separate: it installs client configuration.

Applications repeat the same transport setup around the official Go MCP SDK.
This package shares the stateless JSON transport,
body bounds, cross-origin protection, request cancellation and draining. The
application still owns authentication, authorization, host validation, tool
registration, backend resources and the listener's TLS/proxy policy.

```go
// Mount in the mux already used by the application's HTTP server.
func mountMCP(
    mux *http.ServeMux,
    authenticate func(http.Handler) http.Handler,
    newServer func(*http.Request) *mcp.Server,
) (*mcphttp.Handler, error) {
    endpoint, err := mcphttp.New(newServer, mcphttp.Options{
        Authenticate: authenticate,
        MaxRequestBodyBytes: 4 << 20,
    })
    if err != nil {
        return nil, err
    }
    mux.Handle("/mcp", endpoint)
    return endpoint, nil
}
```

Import `net/http`, `github.com/modelcontextprotocol/go-sdk/mcp` and
`go.kenn.io/kit/mcphttp`. Pass the service's existing authentication middleware:
it must authenticate **every** request, reject failures before calling `next`,
and preserve the request context. It may put caller identity in that context.
Apply the service's host allowlist and authorization policy there as well.
`nil` authentication is a construction error; Kit reads no credentials and
offers no unauthenticated mode. Forwarded headers are not trusted by Kit.

`newServer` must return a **fresh** SDK server for each request. Read identity
and policy from the authenticated request and capture them when registering
tools. Backend clients may be shared when safe; mutable SDK servers must not
be shared with other requests or with stdio. Kit adds receiving middleware
that cancels tool work when the HTTP request disconnects, including for
older protocols where the SDK otherwise detaches cancellation. SDK context
values and explicit protocol cancellation are preserved.

The endpoint retains no MCP sessions and ignores caller-supplied session IDs.
GET and DELETE are rejected by the stateless SDK transport. Stateful HTTP,
resumable streams and server-initiated requests need the SDK directly. The
application continues to own its stdio command and server construction.

During shutdown, call `endpoint.Shutdown(ctx)` with a bounded context before
closing tool dependencies. It stops new admissions with 503 and waits for
admitted requests. If the deadline expires, it cancels tool contexts and
returns the context error. Handlers must honor cancellation. Then finish the
service's normal `http.Server.Shutdown`/`Close` sequence; Kit never closes a
shared listener. If shutdown times out, wait for application work to retire
before releasing resources it may still use.

Ship the application normally in its existing container and mount this route
behind the same authenticated HTTPS access policy as the API. No extra
container, port publication, network trust exception or deployment change is
part of this package. SDK localhost rebinding checks remain enabled, browser
cross-origin writes are rejected, and all endpoint responses use `no-store`.

Tests use synthetic identities and local HTTP servers only.

For optional website/icon metadata shared with stdio and image-only public
routes, see [`mcpidentity`](../mcpidentity/README.md). Mount its image handler
separately; the authentication requirement on this endpoint remains unchanged.
