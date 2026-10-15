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
- Local key components beginning with `.objectstore-` are reserved
  case-insensitively for staging. Reject them in all public point operations,
  while preserving their inventory visibility; a staging file cannot receive
  an immutable receipt or be removed through Delete.
- Listing preserves foreign entries and grants no deletion authority.
  Ordinary tests use temporary files or loopback fixtures, never ambient
  credentials, provider endpoints, or application state.
- S3 credentials and custom endpoints are explicit. Construction performs no
  mutating probes. Conditional PUT and multipart completion never fall back to
  overwrite. Reconcile SDK retries as well as the initial completion attempt.
- S3 conflict reconciliation uses full-object SHA-256 or revision-guarded full
  readback. CRC32C and composite digests cannot establish an existing key's
  identity. `AllowReadback` controls routine upload fallback only.
- S3 inspection, readback, SDK retries, and multipart abort share the caller's
  remaining context. Preserve uncertainty across SDK attempts; an exhausted
  context after final dispatch returns outcome-unknown with its cause.
- S3 revisions use ETags only as conditional operation tokens, never as content
  identity. Provider support needs live contract validation; loopback fixtures
  and RustFS conformance do not establish AWS or other provider support.
- GCS uses explicit HMAC credentials with XML resumable uploads and generation
  preconditions. A routine CRC32C receipt proves upload integrity; reconciliation
  always binds a full SHA-256 readback to the observed generation.
- GCS session URLs are capabilities and must stay out of public errors. Abort
  only the current operation's incomplete session with the caller's context;
  never abort a possibly completed publication to resolve uncertainty.
- Live GCS contract tests require separate authorization and explicit disposable
  bucket/prefix, endpoint, and HMAC settings. Ordinary tests use loopback only.
- GCS inventory uses XML list V2. Decode requested URL-encoded object names
  once and preserve opaque continuation tokens unchanged.
