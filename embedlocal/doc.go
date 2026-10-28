// Package embedlocal finds or starts a local embeddings server.
// Call Endpoint only when the app has no configured endpoint. It prefers Ollama
// on its usual loopback port when nomic-embed-text or its latest tag is installed.
// The returned /v1 base URL leaves model names and input recipes to the caller.
// Obtain download consent before supplying an executable when model assets may
// be fetched. First startup can take up to two minutes while the model loads.
// Startup diagnostics live in kennembed.log in the shared data directory.
package embedlocal
