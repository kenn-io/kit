// Package embedlocal finds or starts a local embeddings server.
// Call Endpoint when the app has no configured endpoint or the user chooses
// kennembed. By default, it prefers Ollama on its usual loopback port when
// nomic-embed-text:latest is installed.
// Set Options.SkipOllama to skip Ollama and select kennembed.
// The returned /v1 base URL leaves model names and input recipes to the caller.
// Obtain download consent before supplying an executable when model assets may
// be fetched. Allow time for the first model download with a context deadline.
// Without a caller deadline, startup waits at most two minutes. A detached
// startup continues loading after cancellation, and later callers can join it.
// Startup diagnostics live in kennembed.log in the shared data directory.
//
// Call Inspect to check the shared installation and Ollama before presenting
// download choices. Its approximate DownloadBytes uses file types and sizes,
// without hashing model contents. Install still verifies cached model contents.
// Installing includes an installer waiting for confirmation. The snapshot is
// advisory and leaves enabled settings, endpoints, and indexing progress to apps.
package embedlocal
