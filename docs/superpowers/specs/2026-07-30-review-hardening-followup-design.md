# Multi-Location Read Review Hardening Follow-up Design

## Goal

Resolve the seven findings from the combined review at `6a1d0d4` while
preserving exact catalog authority, bounded resource use, replica fallback,
and the existing internal legacy resolver behavior.

## Design

### Bounded publication verification

Filesystem and S3 pack publication verification will use
`pack.Reader.OpenBlob` for every footer entry and fully verify and close each
stream. Both readers will receive the configured container, footer, entry,
raw, stored, and decoder-window limits. Verification will reject compressed
frames whose decoder window exceeds policy and frames whose decoded length
differs from the footer before publication succeeds.

The existing uncommitted S3 `OpenBlob` change is retained and extended with
decoded-length coverage. Filesystem verification will adopt the same bounded
streaming pattern and accept a context so cancellation remains effective
during read-back.

### Secure S3 endpoint admission

`s3store.Config` will add `AllowInsecureTransport`. Custom HTTPS endpoints
remain accepted by default. A custom HTTP endpoint is rejected unless the
caller explicitly enables this option; the opt-in may be used for loopback,
private-network, or other intentionally cleartext S3-compatible services.
The conformance test will opt in for its local service. Empty endpoints
continue to use the AWS SDK's default secure endpoint resolution.

Endpoint validation will occur before AWS configuration or client
construction so invalid transport policy cannot create a usable backend.

### Terminal S3 integrity classification

The S3 pack stream wrapper will classify terminal pack checksum, corruption,
and blob-mismatch failures from `Read`, `Verify`, and `Close` as
`packstore.ErrPhysicalCorrupt`. This lets health tracking demote the damaged
generation on the next read. Context cancellation, ordinary I/O failures,
and an intentional early close that reports
`pack.ErrVerificationIncomplete` remain unclassified.

### One-time migration refresh

`NewMultiStore` will enable the existing one-time resolution retry already
used by the legacy store. A retry occurs only when every candidate in the
first resolution is exhausted with a physical-missing headline. The store
resolves again, retries only if the authoritative resolution changed, and
never loops more than once.

### Exact loose-location authority

The zero-valued loose-location sentinel will carry an unexported marker that
only the internal legacy resolver can set. Ordinary `NewMultiStore` resolvers
must provide an explicit raw or zstd encoding and catalog sizes, including
for empty content. `ReadLocation.Validate` and `FilesystemBackend.OpenLoose`
will both reject an unmarked zero-valued location, preventing direct backend
use from bypassing the resolver boundary.

This changes the new multi-location API in place. It does not add another
compatibility adapter or preserve acceptance of ambiguous external
locations.

### Pack publication size admission

Both publication backends will derive an effective maximum as the smaller of
the caller's positive `PublishOptions.MaxBytes` and the configured
`Limits.PackBytes`; zero selects the configured limit and negative values
remain invalid. A known expected size above that maximum is rejected before
creating staging state or starting multipart upload.

Unknown-size filesystem input may be copied only into staging through the
effective bound. Unknown-size S3 input may upload bounded multipart parts,
but exceeding the limit aborts the multipart upload before completion, so no
canonical object is created. Read-back remains an independent defense, not
the first enforcement point.

### Oversized-replica fallback

An S3 pack whose container `ContentLength` exceeds the configured pack limit
is candidate-specific corrupt physical state. The returned error will retain
the structured container `LimitError` and also wrap
`packstore.ErrPhysicalCorrupt`, allowing multi-location resolution to try a
healthy replica and health tracking to demote the oversized generation.

Per-blob raw, stored, and decoder-window limits remain logical policy errors
and do not trigger replica fallback because an equivalent blob in another
container cannot satisfy the same configured logical limit.

## Testing

Focused regression coverage will prove:

- filesystem and S3 publication reject excessive decoder windows and decoded
  lengths through streaming verification;
- HTTPS is accepted by default, HTTP is rejected by default, and explicit
  insecure transport enables the conformance endpoint;
- S3 terminal pack integrity errors are classified while early close is not;
- a changed resolver result is retried once after the first location
  disappears;
- external zero-valued loose locations are rejected while the internal
  legacy resolver continues to operate;
- known and streamed oversized pack publications never enter either
  canonical namespace;
- an oversized S3 replica preserves its limit details and falls through to a
  healthy candidate; and
- logical per-blob limits remain terminal.

Final verification will include focused package tests, the full Go suite,
build, vet, scoped lint, race tests, S3-compatible conformance, and the
existing Linux 386, Plan 9, and Windows portability checks.
