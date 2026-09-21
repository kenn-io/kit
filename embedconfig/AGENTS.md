# embedconfig invariants

- Keep model, role, and input settings in their own types. Do not collapse
  them into one flat configuration struct of scalars.
- Do not default dimensions, truncation, retrieval limits, or generation
  serving. Those choices change compatibility or recall.
- `ApplyDefaults` may fill batch size 64, transport timeout 30s, and a 32 MiB
  response cap. It must not invent a model, a dimension, or a chunk limit.
- `VectorIdentity` includes model, metric, normalization, pooling, role
  affixes and formatters, input type, encoding format, requested dimensions,
  and the endpoint only when `PinEndpoint` is set.
- `InputIdentity` includes the vector identity plus recipe, tokenizer,
  content selection, and token-window limits. Batch size, timeout, response
  cap, retrieval budgets, serving policy, and `TrustPrivateNetwork` stay out
  of both identities.
- API keys are not fields of these types. Callers resolve secrets and pass
  them to the HTTP client.
- `vector.Split` remains a rune window helper. Token limits here do not
  require that splitter.
