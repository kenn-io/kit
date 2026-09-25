# embedclient invariants

- Accept a caller-owned `*http.Client` without modifying it. A nil client
  uses Transport.Timeout from embedconfig.
- Do not retry ordinary failures. Put `Retry-After` on `APIError` and let
  the caller decide. `OllamaMetalRecovery` is the one exception: keep usable
  vectors and recover the others through Ollama's native embed route.
- Do not return a short vector slice. A failed request fails the call.
- Reorder by the per-request `index`. Do not treat that index as an offset
  into a larger `vector.EncodeBatched` input.
- `Embed` applies role prefixes to raw content. `EncodeFunc` sends the
  strings it is given, because fitted text already includes them.
- A 400 is `InputRejected`. A 401 or 403 is `CredentialsRejected` and is
  not a reason to skip one document. 408, 429, and 5xx are `Retryable`. Do
  not copy the provider body into any error.
- A transport failure is a `*TransportError`. Its message stays generic
  because the cause can name internal hosts; `Unwrap` keeps the cause.
- An invalid vector is a `*VectorError` whose `Index` is the caller's input
  position, not the position inside one request. Recovery reads the index
  with `errors.AsType`, never by parsing the message.
- A request waiting for Ollama recovery on another request returns when its
  own context ends.
- Normalize with L2 only when the model normalization says so. Always reject
  a non-finite component, a null component, or a zero norm.
- Do not copy provider bodies into errors.
- Pack same-role inputs with the effective item cap. When both token fields
  are positive, the cap is min(Items, MaxTokens/InputTokenUpperBound), and
  at least 1 when Items and that quotient are at least 1. New returns an
  error when InputTokenUpperBound is greater than MaxTokens.
- New accepts only the cosine metric. EncodeFunc serves the cosine pipeline
  and must not silently accept dot_product or l2.
- Encoding format must be empty, float, or base64. An embedding is a JSON
  array of finite numbers or a base64 string of little-endian float32 values.
