# embedclient invariants

- Accept a caller-owned `*http.Client` without modifying it. A nil client
  uses Transport.Timeout from embedconfig.
- Do not retry. Put `Retry-After` on `APIError` and let the caller decide.
- Do not return a short vector slice. A failed request fails the call.
- Reorder by the per-request `index`. Do not treat that index as an offset
  into a larger `vector.EncodeBatched` input.
- Apply role prefixes here. Count those same strings in embedfit.
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
