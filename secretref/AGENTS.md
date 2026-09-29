# secretref invariants

- One configuration value names a secret and its source: `env:NAME`,
  `file:PATH`, or the secret itself. Callers declare a single key of type
  `Ref`; do not add parallel `_env` or `_file` keys.
- A value shaped like `scheme:rest` whose scheme is an unknown lowercase word
  is an error, never an inline secret. That keeps a future scheme, such as a
  secret store, from changing what an existing value means. Add a scheme by
  extending `parse` and `Resolve`, with tests.
- A source that yields nothing is not an error. `Resolve` returns an empty
  `Value` and a `Reason`, so a caller can keep running without the secret.
  Only a malformed reference is an error.
- `Source` and `Reason` never contain the secret. They may name the variable
  or the configured path.
- Files open through `safefileio.OpenCurrentUserFile` and must pass
  `safefileio.ValidatePrivateCurrentUserFile`. Do not follow symlinks, block
  on a FIFO, or repair permissions here.
