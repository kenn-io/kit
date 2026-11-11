# secretref invariants

- One configuration value holds a secret or names its source. A string is
  always the literal secret; it is never parsed or expanded. Every other
  source is a field of the table form (`env`, `file`). Callers declare a
  single key of type `Ref`. `FromFields` adapts existing sibling keys;
  `FromEnv` adapts NAME/NAME_FILE with presence-based conflicts, no precedence.
- Add a source, such as a secret manager or a credential helper, as a new
  table field. Update `Validate`, `UnmarshalTOML`, `UnmarshalJSON`,
  `MarshalTOML`, and `Resolve` together, with tests. Never give the string
  form a new meaning.
- `Ref` must decode with any TOML library: tagged exported fields and
  `UnmarshalText` cover libraries that decode tables into structs, and
  `UnmarshalTOML` covers decoders that try `UnmarshalText` on tables. Do not
  import a TOML library here.
- A table names exactly one source. More than one, or an unknown field, is an
  error. Field-only TOML decoders need strict mode/undecoded-key checking at
  their boundary: Validate cannot see discarded keys. Validation performs no I/O.
- An unset reference resolves successfully without a secret. A configured
  environment or file source that is missing, empty, or unreadable is an
  error. Callers decide whether to keep running without the provider.
- Exact-empty literals remain unset; blank literals return a reason. Preserve
  literal/env bytes and file bytes except trailing
  CR/LF. Bound raw files to 64 KiB by both stat and bounded read.
- `Secret` redacts direct formatting, JSON and slog; formatting an unexported
  field containing it can expose the value.
  SourceError.Source may name a selector; Reason never contains selectors or
  secret values. Do not forward raw OS/decoder errors that can expose values.
- Files open through `safefileio.OpenSecretFile`, whose trusted path permits
  controlled links and root-owned mounted files. Never repair permissions or
  block on a FIFO.
- Use ResolveContext when a context is available; Resolve is a compatibility
  wrapper. Check cancellation between I/O steps without promising kernel-call
  interruption.
- The literal secret is also the exported `Value` field, so encoders that
  write struct fields instead of calling `MarshalTOML` still keep it; they
  write `{ value = "..." }`, which every decoder reads back.
