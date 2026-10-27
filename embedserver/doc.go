// Package embedserver runs a loopback OpenAI embeddings endpoint for
// nomic-embed-text v1.5. Callers format role prefixes before sending text.
// Inputs truncate at 2,048 tokens, including CLS and SEP. Inference mean-pools
// attended tokens and L2-normalizes the resulting 768-dimensional vector.
// Smaller dimensions truncate and normalize that vector again.
//
// Serve holds one owner lock while downloading, loading, and serving. Runtime
// records appear only after readiness. Cancellation drains requests before
// releasing native tensors, the session, and the ONNX Runtime library.
// ONNX Runtime 1.23.2, API 23, must be installed beside the executable or supplied
// explicitly through Options.Library. Runtime installation belongs to the caller.
package embedserver
