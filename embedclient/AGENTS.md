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
