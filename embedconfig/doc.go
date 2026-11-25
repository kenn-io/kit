// Package embedconfig holds reusable embedding settings.
//
// Applications own files, flags, and persistence. This package owns the value
// types, defaults, and validation those applications were copying, and
// Embedder, the standard configuration-file shape for one embedding endpoint,
// so every application reads the same keys.
//
// VertexBaseURL constructs a Vertex endpoint for a location. Set Deployment.API to
// APIVertex to select its request protocol. Embedder.API carries the same
// choice through Parts. The default is APIOpenAI. Vertex prediction calls use
// RETRIEVAL_DOCUMENT for documents and RETRIEVAL_QUERY for queries, request Model.Dimensions, and return float vectors, so
// InputTypeMode, RequestDimensions, and EncodingFormat have no effect there.
// A Vertex request may carry fewer than Batch.Items inputs.
//
// The types stay separate on purpose. Model, role, and input settings can
// change vectors or force a rebuild. Batch size and timeouts do not.
// embedmodel.Descriptor combines them and computes the identities.
//
// Dimensions and truncation have no default. Batch size defaults to 32
// items, which every common embedding server and provider accepts, and the
// transport timeout defaults to 30 seconds. Deployments that need other
// values, such as 128 items or a 45 second timeout, set them explicitly.
package embedconfig
