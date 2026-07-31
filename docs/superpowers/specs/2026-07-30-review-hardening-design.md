# Multi-Location Read Review Hardening Design

## Goal

Resolve the five findings from the combined review at `620687a` and restore
the two failing portability jobs from CI run `30593899375` without widening
Kit's public API or weakening its fail-closed storage behavior.

## Design

### Bounded S3 pack handling

The S3 backend will normalize an all-zero `Config.Limits` value to
`packstore.DefaultLimits` and reject any other non-positive limit. Pack
downloads will reject a missing, negative, or over-limit `ContentLength`
before creating a staging file. Every staged pack reader, including
publication read-back, will receive container, footer, entry-count, raw,
stored, and decoder-window limits derived from the normalized configuration.

Read-back verification will compare `ContentLength` with the expected
publication size before staging and will copy through an
`expectedSize + 1` reader. This prevents a conflicting or malformed remote
object from causing an unbounded transfer even when its metadata is absent or
incorrect.

### Inventory key safety

Pack inventory keys will validate the candidate pack ID with
`pack.IsValidPackID` before using its first two characters for shard
validation. Malformed keys remain unknown inventory entries and never panic.

### Terminal filesystem stream classification

Filesystem backends will return a small verified-stream decorator. It will
delegate `Read`, `Verify`, `Verified`, and `Close`, classifying terminal
integrity errors from the first three error-returning methods through
`classifyPhysicalError`. This gives bounded candidate resolution and health
observation the same typed physical-corruption evidence already produced by
open-time failures. Early-close verification-incomplete errors remain
unclassified.

### Ownership marker symmetry

`MarshalOwnership` will reject a canonical JSON representation longer than
`maxOwnershipMarkerBytes`. Both filesystem and S3 ownership replacement use
this function, so neither backend can publish a marker that `ParseOwnership`
later refuses solely because of size.

### Multipart deduplication cleanup

When conditional multipart completion returns HTTP 412, the upload remains
incomplete and the existing deferred cleanup will call
`AbortMultipartUpload` before `multipartPublish` returns the deduplicated
result. Successful completion alone suppresses abort.

### Portability repairs

The hardened `safefileio` implementations that require Unix ownership and
open flags will use the `unix` build constraint. A separate
`!unix && !windows` implementation will define the same API but return an
explicit unsupported-platform error, allowing Plan 9 builds to compile while
remaining fail closed.

Windows pack readers deliberately request delete sharing, so retiring a pack
with only an internal active reader can succeed while that reader retains its
exact descriptor. The cross-platform movement test will assert this behavior
on every OS. Existing Windows-only tests continue to prove that an external
handle which denies delete sharing returns `ErrPackRetirementDeferred`.

## Testing

Focused regression tests will cover:

- S3 limit normalization and rejection;
- over-limit S3 `ContentLength` rejection before range GETs;
- reader-option enforcement for staged S3 packs;
- bounded S3 publication read-back;
- malformed short pack inventory keys;
- classification of terminal filesystem stream errors;
- oversized ownership encoding rejection;
- multipart 412 abort;
- Plan 9 compilation; and
- active-reader retirement semantics, including the existing Windows
  sharing-violation tests.

Repository verification will follow CI: formatting, package tests, full tests,
build, vet, lint, race tests for streaming packages, Linux 386 compilation,
Plan 9 compilation, and available Windows cross-compilation or execution.
