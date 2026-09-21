// Package embedclient calls an OpenAI-compatible text embeddings endpoint.
//
// The caller owns the HTTP client when it supplies one. This package clones
// that client, pins requests to the configured origin, and can attach a bearer
// token. It does not retry. A non-2xx response, a short body, or a vector that
// fails validation fails the whole call. Response indexes are applied inside
// each request. A missing index on every item means the provider kept request
// order.
//
// Text is the only encoded form. Image and file content returns
// embedmodel.ErrUnsupportedContent so those callers can keep their own path.
// vector.EncodeFunc remains available through EncodeFunc.
package embedclient
