# embedmodel invariants

- Keep vector-space, input-recipe, and lexical-analyzer identities separate.
  `Generation` carries the first two. It must not carry the lexical identity.
- Do not require `[]string` or `vector.Split`. Text content, text parts, and
  caller-prepared source spans are all valid inputs.
- Image and file parts validate here and stay caller-owned. Do not add a
  binary embedding protocol in this package.
- Formatter names are identity labels. `Format` applies only the literal
  role prefix and suffix.
- Source spans refer to the caller source. They are not required to fall
  inside the formatted embedding string.
