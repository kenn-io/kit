// Package embedlocal finds or starts a local embeddings server.
// Call Endpoint when the app has no configured endpoint or the user chooses
// kennembed. By default, it prefers Ollama on its usual loopback port when
// nomic-embed-text:latest is installed.
// Set Options.SkipOllama to skip Ollama and select kennembed.
// The returned /v1 base URL leaves model names and input recipes to the caller.
// Select FaceModel to discover kennembed directly. A missing requested model
// returns ErrNotFound so the caller can install it with app-owned consent.
// Without a caller deadline, startup waits at most two minutes. A detached
// startup continues loading after cancellation, and later callers can join it.
// Startup diagnostics live in kennembed.log in the shared data directory.
package embedlocal
