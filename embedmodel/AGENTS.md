# embedmodel invariants

- Keep vector-space, input-recipe, and lexical-analyzer identities separate.
  `Generation.Params` carries only the vector-space id, under `vector_space`.
  It must not carry `input_recipe` or the lexical identity. `InputIdentity`
  stays the separate input id.
- Do not require `[]string` or `vector.Split`. Text content, text parts, and
  caller-prepared source spans are all valid inputs.
- Only cosine validates. `MetricDotProduct` and `MetricL2` remain named, and
  `Model.Validate` rejects them because stored vectors are cosine-only.
- Image and file content validates here and stays caller-owned. `EmbedText`
  returns `ErrUnsupportedContent` for those top-level kinds even when `Text`
  is set, and for parts of those kinds. Do not add a binary embedding
  protocol in this package.
- Formatter names are identity labels. `Format` applies only the literal
  role prefix and suffix.
- Source spans refer to the caller source. They are not required to fall
  inside the formatted embedding string.
