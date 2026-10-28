// Package embedlocal finds or starts a local embeddings server.
// Call Endpoint only when the app has no configured endpoint. Obtain download
// consent first when starting the executable may fetch model assets.
// The returned /v1 base URL leaves model names and input recipes to the caller.
package embedlocal
