// Package embedlocal finds or starts a local embeddings server.
// Call Endpoint when the app has no configured endpoint or the user chooses
// kennembed. By default, it prefers Ollama on its usual loopback port when
// nomic-embed-text:latest is installed.
// Set Options.SkipOllama to skip Ollama and select kennembed.
// Endpoint and Inspect probe Ollama for at most 500 milliseconds and refuse redirects.
// The returned /v1 base URL leaves model names and input recipes to the caller.
// Select FaceModel to discover kennembed directly. A missing requested model
// returns ErrNotFound so the caller can install it with app-owned consent.
// Without a caller deadline, startup waits at most two minutes. A detached
// startup continues loading after cancellation, and later callers can join it.
// Startup diagnostics live in kennembed.log in the shared data directory.
// For a built-in server endpoint, use BatchItems and RequestTimeout when the
// user has not set their own values, and send one request at a time.
//
// Call Inspect to check the shared installation and Ollama before presenting
// download choices. Its approximate DownloadBytes uses file types and sizes,
// without hashing model contents. Install still verifies cached model contents.
// Installing means approved work holds the install lock and a staging directory.
// The advisory snapshot leaves settings, endpoints, and indexing progress to apps.
//
// Call RecordChoice only after a successful explicit turn-on with the built-in server.
// At startup, call ChoiceRecorded to read the shared choice. Only the exact bytes
// `yes\n` under `~/.kenn/embeddings` count, regardless of `Options.DataDir`.
// On a read error, leave the shared choice off. Apps retain their own provider
// and enabled settings.
package embedlocal
