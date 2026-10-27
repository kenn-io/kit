// Package embedserver serves nomic-embed-text v1.5 on loopback. Callers supply
// role prefixes. String or string-array inputs truncate at 2,048 WordPiece tokens
// including CLS and SEP. Indexed float or base64 float32 vectors use mean pooling
// and L2 normalization after selecting the requested dimensions.
//
// Serve prints its ready URL to stderr and holds the owner lock until context
// cancellation drains requests, releases native resources, and removes its record.
// kennembed runs in the foreground until Ctrl-C or SIGTERM. Its flags select the
// data dir, listen address, and ONNX Runtime 1.23.2, API 23, library. The caller
// installs that library beside the executable or supplies Options.Library.
// Model downloads last until completion or context cancellation.
package embedserver
