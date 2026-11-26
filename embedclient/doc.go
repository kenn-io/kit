// Package embedclient calls Kit, OpenAI-compatible, and Vertex embedding endpoints.
//
// Vertex predict requests use task type RETRIEVAL_DOCUMENT for document roles
// and RETRIEVAL_QUERY for query roles. Gemini embedding 2 uses embedContent
// and ignores task types, so New requires a role prefix or formatter that
// carries its task, such as "task: search result | query: " for queries and
// "title: none | text: " for documents. Both request Model.Dimensions and
// always return float vectors. InputTypeMode, RequestDimensions, and
// EncodingFormat do not change a Vertex request.
//
// Vertex sends up to 250 inputs per request for gemini-embedding-001,
// text-embedding-005, and text-multilingual-embedding-002, and one for other
// models, further capped by Batch.Items. A Vertex 400 that names a
// token limit is an input rejection; a request with more than one input is split
// in halves when it receives one. Set Batch.Items so a request stays under
// 20,000 tokens to avoid failed first attempts.
// GoogleHTTPClient supplies Application Default Credentials; pass it as
// Options.HTTP.
//
// The caller owns the HTTP client when it supplies one. This package clones
// that client, pins requests to the configured origin, and can attach a bearer
// token. A multi-input request that times out is split into halves, recursively,
// while the caller's context remains active. Single-input timeouts and other
// eligible failures are retried only when the caller sets Options.Retry.
// A non-2xx response, a short body, or a vector that fails validation fails
// the whole call.
// Response indexes are applied inside each request. A missing index on every item means the
// provider kept request order.
//
// Ollama on Apple Metal can answer HTTP 200 with a vector that is not a real
// number. Set OllamaMetalRecovery to keep the usable vectors and send only
// the bad inputs to Ollama's native embed route. That request unloads the
// current runner, retries it once, and then encodes those inputs once with
// the GPU off. Those recovery requests use a 30 minute timeout instead of
// the per-request timeout, because a CPU pass can take many minutes. The endpoint path must end in /v1. The switch is not part of the
// vector identity.
//
//	client, err := embedclient.New(embedclient.Options{
//	    Model: embedconfig.Model{
//	        Name: "nomic-embed-text", Dimensions: 768,
//	        Metric: embedconfig.MetricCosine,
//	        Normalization: embedconfig.NormalizationNone,
//	    },
//	    Deployment: embedconfig.Deployment{
//	        BaseURL: "http://127.0.0.1:11434/v1",
//	    },
//	    OllamaMetalRecovery: true,
//	})
//
// Leave the switch off for every other endpoint.
//
// New accepts only a cosine metric. A token budget lowers how many inputs
// share one request. Response embeddings are a JSON array of finite numbers
// or a base64 string of little-endian float32 values. The encoding format is
// empty, float, or base64.
//
// Failures are typed. APIError reports the status, a Reason derived from the
// status and the provider's error code or message, and whether a retry may
// help. The provider body itself is never kept. TransportError keeps the
// network cause behind a generic message: "embed request timed out" for a
// timeout and "embed request failed" for other transport failures.
// VectorError names the input whose vector failed validation, and matches
// ErrInvalidVector.
//
// Embed and EmbedTexts apply the role prefix and suffix to raw content.
// EncodeFunc sends its strings unchanged, so text from embedfit.Prepared is
// not wrapped a second time.
//
// A Kit client for Gemini embedding 2 requires a role prefix or formatter when
// Embed or EmbedInputs formats text. Encode accepts preformatted text.
//
// An Input carries a role and one payload: Text, or MediaType with Data.
// EmbedInputs adds role affixes to text inputs. Encode sends inputs as given
// and implements Backend. Neither joins, trims, nor splits text. Kit services
// receive inputs over the HTTP contract at
// https://github.com/kenn-io/kit/blob/main/docs/embedding-http.md, and Check
// tests a service against it. A media input goes in its own request to a Kit
// service or to Vertex gemini-embedding-2. OpenAI-compatible services and
// other Vertex models reject media with embedmodel.ErrUnsupportedContent. The
// same error identifies a Kit service that reports unsupported content. Embed
// remains text-only.
package embedclient
