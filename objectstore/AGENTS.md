# Immutable object storage

- Keep shared values and errors provider-neutral. Consumers own interfaces,
  key naming, catalog state, and retention decisions.
- Publication returns a verified receipt or a zero receipt with an error.
  Validate the source before attempting publication. A verified identical
  retry succeeds; only a proven stored difference is ErrExists.
- Uncertain publication or verification wraps ErrOutcomeUnknown and its cause.
  Never overwrite or delete a key to resolve uncertainty.
- Local retries verify bytes and repeat file and ancestor directory sync.
  Preserve atomicfile's supported-platform semantics and no-copy behavior.
- Local roots and their ancestors are caller-owned, stable directories.
  Local revisions are unsupported; do not invent file identity guarantees.
- Listing preserves foreign entries and grants no deletion authority.
  Ordinary tests use temporary files or loopback fixtures, never ambient
  credentials, provider endpoints, or application state.
