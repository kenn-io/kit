// Package embedserver serves nomic-embed-text v1.5 and AuraFace-v1 on loopback.
// Text callers supply role prefixes. The built-in model truncates text at
// 2,048 WordPiece tokens including CLS and SEP on both /v1/embed and
// /v1/embeddings. Handler serves the Kit request at paths ending in /embed
// and OpenAI text requests at paths ending in /embeddings, both from one
// embedclient.Backend. kennembed serves /v1/embed through Handler and keeps
// its own /v1/embeddings handler and usage response. The HTTP contract is at
// https://github.com/kenn-io/kit/blob/main/docs/embedding-http.md.
// Indexed float or base64 float32 vectors use mean pooling and L2
// normalization after selecting the requested dimensions.
//
// Serve prints its ready URL to stderr. Cancellation closes connections and waits
// for active inference before releasing native resources and removing its record.
// kennembed runs in the foreground until Ctrl-C or SIGTERM with the default
// data dir and listen address. ONNX Runtime must provide API 23 or newer,
// tested with 1.23.2. The caller installs that library beside the executable
// or supplies Options.Library.
// The -listen, -data-dir, and -library flags override those defaults.
// Install models with embedlocal before inference. The server loads installed
// models on demand and makes no network requests.
package embedserver
