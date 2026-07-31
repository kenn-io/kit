# Multi-Location Read Review Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve five storage review findings and two CI portability failures.

**Architecture:** Keep fixes local to the owning packages. S3 receives private
limit and test-transport helpers, filesystem streams receive a private
classification decorator, ownership encoding becomes size-symmetric, and
unsupported operating systems compile with explicit fail-closed stubs.

**Tech Stack:** Go, AWS SDK v2, testify, GitHub Actions portability targets.

## Global Constraints

- Do not add public APIs.
- Preserve app-neutral package boundaries.
- Use testify for changed tests.
- Keep Unix, Windows, and unsupported-platform behavior explicit.
- Do not invoke `roborev review`.

---

### Task 1: S3 pack limits and bounded read-back

**Files:**
- Modify: `packstore/s3store/backend.go`
- Modify: `packstore/s3store/reader.go`
- Modify: `packstore/s3store/publication.go`
- Create: `packstore/s3store/backend_test.go`
- Create: `packstore/s3store/reader_test.go`
- Create: `packstore/s3store/publication_test.go`

**Interfaces:**
- Consumes: `packstore.Limits`, `pack.ReaderOptions`, AWS S3 HTTP behavior.
- Produces: normalized private backend limits and bounded pack staging.

- [ ] Write tests that reject partial invalid limits and default an all-zero
  limit set.
- [ ] Run the focused tests and confirm they fail because S3 retains zero or
  invalid limits.
- [ ] Normalize and validate limits in `New`.
- [ ] Write an HTTP-backed test where `HeadObject.ContentLength` exceeds
  `PackBytes` and assert no range GET occurs.
- [ ] Run it and confirm the current downloader starts or permits the
  over-limit transfer.
- [ ] Reject over-limit metadata before creating the staging file.
- [ ] Write pack fixtures that exceed footer, entry, raw/stored, or window
  policy and assert reader limit errors.
- [ ] Run them and confirm the current unbounded reader accepts the fixtures.
- [ ] Construct `pack.ReaderOptions` from every configured S3 limit for both
  ordinary reads and publication verification.
- [ ] Write read-back tests that expose a conflicting object larger than the
  expected publication and assert only the bounded prefix can be consumed.
- [ ] Run them and confirm the current `io.CopyBuffer` is unbounded.
- [ ] Add metadata checks and `expectedSize + 1` copy bounds.
- [ ] Run all `packstore/s3store` tests.

### Task 2: Safe S3 key parsing and multipart cleanup

**Files:**
- Modify: `packstore/s3store/keys.go`
- Modify: `packstore/s3store/keys_test.go`
- Modify: `packstore/s3store/publication.go`
- Modify: `packstore/s3store/publication_test.go`

**Interfaces:**
- Consumes: canonical object-key grammar and conditional multipart completion.
- Produces: panic-free inventory parsing and no leaked deduplicated uploads.

- [ ] Add malformed short pack-key cases and confirm the current parser
  panics.
- [ ] Validate pack IDs before shard slicing.
- [ ] Add a controlled S3 multipart sequence that returns 412 from completion
  and records abort.
- [ ] Run it and confirm abort is currently suppressed.
- [ ] Suppress deferred abort only after successful completion.
- [ ] Run focused key and publication tests.

### Task 3: Terminal stream integrity classification

**Files:**
- Modify: `packstore/filesystem_backend.go`
- Modify: `packstore/filesystem_backend_test.go`

**Interfaces:**
- Consumes: `VerifiedReadCloser` and `classifyPhysicalError`.
- Produces: a private decorator that classifies `Read`, `Verify`, and `Close`.

- [ ] Add focused tests for late `ErrContentMismatch` and `pack.ErrCorrupt`
  from each terminal method, plus an incomplete-close control case.
- [ ] Run them and confirm the current returned streams expose unclassified
  integrity errors.
- [ ] Implement the verified-stream decorator and return it from
  `OpenLoose` and `OpenPack`.
- [ ] Run package tests and confirm bounded fallback/health behavior remains
  green.

### Task 4: Ownership marker publication limit

**Files:**
- Modify: `packstore/ownership.go`
- Modify: `packstore/ownership_test.go`

**Interfaces:**
- Consumes: canonical ownership JSON.
- Produces: encodings guaranteed readable by `ParseOwnership`.

- [ ] Add a value whose canonical representation exceeds 4096 bytes and assert
  `MarshalOwnership` rejects it.
- [ ] Run it and confirm the oversized encoding currently succeeds.
- [ ] Check encoded length before appending the successful result.
- [ ] Run ownership and backend tests.

### Task 5: Plan 9 fail-closed compilation

**Files:**
- Modify: `safefileio/open_file_unix.go`
- Modify: `safefileio/private_dir_unix.go`
- Modify: `safefileio/userid_unix.go`
- Create: `safefileio/open_file_other.go`
- Create: `safefileio/private_dir_other.go`
- Create: `safefileio/userid_other.go`

**Interfaces:**
- Consumes: `safefileio` public API.
- Produces: Unix implementations only on Unix and explicit unsupported errors
  elsewhere.

- [ ] Reproduce the Plan 9 compile failure.
- [ ] Narrow Unix build constraints to `unix`.
- [ ] Add `!unix && !windows` stubs returning platform-specific unsupported
  errors without touching the filesystem.
- [ ] Compile `pack`, `packstore`, and `safefileio` for Plan 9.

### Task 6: Windows active-reader retirement expectation

**Files:**
- Modify: `packstore/move_test.go`

**Interfaces:**
- Consumes: internal pack readers opened with `FILE_SHARE_DELETE`.
- Produces: a portable lifecycle regression test consistent with the actual
  descriptor contract.

- [ ] Retain the CI log as the failing reproduction.
- [ ] Rename the test to describe retained reader usability and require
  retirement success on every OS.
- [ ] Cross-compile the package tests for Windows.
- [ ] Preserve the Windows-only external sharing-violation tests unchanged.

### Task 7: Final verification

**Files:**
- Modify only files required by failures found during verification.

- [ ] Run `gofmt` and `goimports` on changed Go files.
- [ ] Run focused tests for `packstore`, `packstore/s3store`, and
  `safefileio`.
- [ ] Run `go test ./...`, `go build ./...`, and `go vet ./...`.
- [ ] Run configured lint.
- [ ] Run `go test -race ./pack ./packstore ./backup`.
- [ ] Compile Linux 386 and Plan 9 targets from CI.
- [ ] Cross-compile Windows amd64 package tests.
- [ ] Inspect the final diff for scope, platform behavior, and test quality.
