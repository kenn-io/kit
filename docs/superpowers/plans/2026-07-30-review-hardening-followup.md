# Multi-Location Read Review Hardening Follow-up Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the seven security, integrity, and migration findings from the
review at `6a1d0d4`.

**Architecture:** Enforce transport and publication policy before canonical
S3 or filesystem state can be created, and stream every pack verification
through the bounded reader. Centralize terminal integrity classification in
`packstore`, keep legacy loose-location authority privately marked, and reuse
the existing one-shot resolver refresh.

**Tech Stack:** Go, AWS SDK v2, `pack` streaming readers, testify, MinIO
conformance tests.

## Global Constraints

- Custom S3 endpoints are absolute HTTP(S) URLs; HTTPS is the default and
  HTTP requires explicit `AllowInsecureTransport`.
- `Limits.PackBytes` is an unconditional upper bound even when callers supply
  a larger or zero `PublishOptions.MaxBytes`.
- Container-limit failures are candidate-specific; per-blob limits remain
  logical terminal errors.
- Only the internal legacy resolver may authorize an encoding-zero loose
  location.
- Use testify in every new or changed test.
- Do not add compatibility adapters for the new multi-location API.
- Do not invoke `roborev review`.

---

### Task 1: Shared terminal integrity classification

**Files:**
- Modify: `packstore/location_errors.go`
- Modify: `packstore/location_errors_test.go`
- Modify: `packstore/filesystem_backend.go`
- Modify: `packstore/s3store/reader.go`
- Modify: `packstore/s3store/reader_test.go`

**Interfaces:**
- Consumes: pack integrity sentinels and `packstore.ErrPhysicalCorrupt`.
- Produces: `packstore.ClassifyIntegrityError(error) error`, used by both
  filesystem and S3 verified-stream wrappers.

- [ ] **Step 1: Add failing shared-classifier tests**

Add a table test covering the exact corruption set and controls:

```go
func TestClassifyIntegrityError(t *testing.T) {
	tests := []struct {
		name    string
		input   error
		corrupt bool
	}{
		{"content mismatch", ErrContentMismatch, true},
		{"bad magic", pack.ErrBadMagic, true},
		{"unsupported version", pack.ErrUnsupportedVersion, true},
		{"truncated", pack.ErrTruncated, true},
		{"checksum", pack.ErrChecksum, true},
		{"corrupt", pack.ErrCorrupt, true},
		{"blob mismatch", pack.ErrBlobMismatch, true},
		{"verification incomplete", pack.ErrVerificationIncomplete, false},
		{"context canceled", context.Canceled, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ClassifyIntegrityError(tt.input)
			assert.Equal(t, tt.corrupt, errors.Is(err, ErrPhysicalCorrupt))
			require.ErrorIs(t, err, tt.input)
		})
	}
}
```

- [ ] **Step 2: Run the classifier test and observe RED**

Run:

```sh
go test ./packstore -run '^TestClassifyIntegrityError$'
```

Expected: build failure because `ClassifyIntegrityError` is undefined.

- [ ] **Step 3: Export the shared classifier**

Move only integrity mapping into the exported function and retain
filesystem-only missing-source mapping privately:

```go
func ClassifyIntegrityError(err error) error {
	if err == nil || isCandidateFailure(err) {
		return err
	}
	for _, corrupt := range []error{
		ErrContentMismatch,
		pack.ErrBadMagic,
		pack.ErrUnsupportedVersion,
		pack.ErrTruncated,
		pack.ErrChecksum,
		pack.ErrCorrupt,
		pack.ErrBlobMismatch,
	} {
		if errors.Is(err, corrupt) {
			return errors.Join(ErrPhysicalCorrupt, err)
		}
	}
	return err
}

func classifyPhysicalError(err error) error {
	if err == nil || isCandidateFailure(err) {
		return err
	}
	if isPhysicalSourceNotFound(err) {
		return errors.Join(ErrPhysicalMissing, err)
	}
	return ClassifyIntegrityError(err)
}
```

- [ ] **Step 4: Add failing S3 terminal-stream tests**

Build a real `pack.BlobReader` over a corrupted staged pack and assert that
`packBody.Read` and `packBody.Verify` expose both the original pack error and
`packstore.ErrPhysicalCorrupt`. Add an unconsumed valid stream control:

```go
err := body.Close()
require.ErrorIs(t, err, pack.ErrVerificationIncomplete)
require.NotErrorIs(t, err, packstore.ErrPhysicalCorrupt)
```

Add a multi-store test with the corrupt S3 stream first and a healthy backend
second. After consuming the first stream's terminal corruption, open the hash
again and assert the healthy generation is selected without reopening the
corrupt generation.

- [ ] **Step 5: Run the S3 tests and observe RED**

Run:

```sh
go test ./packstore/s3store -run 'TestPackBody|TestS3TerminalCorruptionDemotesGeneration'
```

Expected: terminal pack errors do not satisfy
`packstore.ErrPhysicalCorrupt`, and the first generation is selected again.

- [ ] **Step 6: Classify S3 terminal methods**

Apply `packstore.ClassifyIntegrityError` to errors returned by
`packBody.Read`, `Verify`, and the joined cleanup in `finish`; leave
`Verified` unchanged:

```go
func (r *packBody) Verify() error {
	err := r.blob.Verify()
	r.finish()
	return packstore.ClassifyIntegrityError(errors.Join(err, r.closeErr))
}

func (r *packBody) finish() {
	if r.once {
		return
	}
	r.once = true
	r.closeErr = packstore.ClassifyIntegrityError(errors.Join(
		r.blob.Close(), r.reader.Close(), os.Remove(r.path),
	))
}
```

Use the same final classification shape in `Read` and `Close`.

- [ ] **Step 7: Run focused tests and commit**

Run:

```sh
go test ./packstore ./packstore/s3store
```

Then commit only this task's files with a rationale-first message.

---

### Task 2: Bounded publication verification

**Files:**
- Modify: `packstore/filesystem_backend.go`
- Modify: `packstore/filesystem_backend_test.go`
- Modify: `packstore/s3store/publication.go`
- Modify: `packstore/s3store/publication_test.go`

**Interfaces:**
- Consumes: `pack.Reader.OpenBlob(context.Context, pack.Entry)` and configured
  reader limits.
- Produces: context-aware, constant-memory publication read-back in both
  backends.

- [ ] **Step 1: Add failing filesystem decoder-window test**

Create a compressed frame with `zstd.WithWindowSize(8<<20)`, append it with
`pack.Writer.AppendEncoded`, configure `BlobBytes` below the frame window, and
publish it through `FilesystemBackend.PublishPack`. Assert:

```go
require.ErrorIs(t, err, ErrBlobTooLarge)
var limit *LimitError
require.ErrorAs(t, err, &limit)
assert.Equal(t, LimitBlobWindowBytes, limit.Dimension)
```

- [ ] **Step 2: Add decoded-length regression fixtures**

For filesystem and S3 verification, create frames whose actual decoded
length is respectively shorter and longer than the footer `RawLen`. Invoke
the real publication verifier and require `pack.ErrCorrupt` plus
`ErrPhysicalCorrupt`; no pack-package behavior test is added.

- [ ] **Step 3: Run publication tests and observe RED**

Run:

```sh
go test ./packstore -run 'TestFilesystemBackendPublishPackRejects(DecoderWindow|DecodedLength)'
go test ./packstore/s3store -run 'TestVerifyPackObjectRejectsDecodedLength'
```

Expected: filesystem accepts the oversized window through `ReadBlob`, and
the decoded-length fixtures expose the current whole-blob path.

- [ ] **Step 4: Stream filesystem verification**

Change the signature to:

```go
func verifyFilesystemPack(
	ctx context.Context,
	path string,
	packID string,
	limits Limits,
	expectedSize int64,
	expectedDigest [sha256.Size]byte,
) (resultSize int64, resultErr error)
```

Pass `ctx` from `PublishPack`, add:

```go
WindowBytes: uint64(max(limits.BlobBytes, int64(1<<10))),
```

and replace `ReadBlob` with:

```go
for _, entry := range reader.Entries() {
	blob, err := reader.OpenBlob(ctx, entry)
	if err != nil {
		return 0, mapPackStreamLimit(err)
	}
	if err := errors.Join(blob.Verify(), blob.Close()); err != nil {
		return 0, mapPackStreamLimit(err)
	}
}
```

Use the package's existing pack-limit translation so public `LimitError`
dimensions remain stable.

- [ ] **Step 5: Retain and complete S3 streaming verification**

Keep the existing local `OpenBlob(ctx, entry)` change in
`verifyPackObject`, preserve context cancellation, structured limit mapping,
and physical-corrupt classification, and add the decoded-length tests from
Step 2.

- [ ] **Step 6: Run focused tests and commit**

Run:

```sh
go test ./packstore ./packstore/s3store
```

Then commit the bounded-verification implementation and tests.

---

### Task 3: Secure S3 endpoint admission

**Files:**
- Modify: `packstore/s3store/backend.go`
- Modify: `packstore/s3store/backend_test.go`
- Modify: `packstore/s3store/backend_integration_test.go`

**Interfaces:**
- Consumes: `Config.Endpoint` and new `Config.AllowInsecureTransport`.
- Produces: absolute HTTP(S)-only endpoint validation before AWS client
  construction.

- [ ] **Step 1: Add failing endpoint table test**

Use `testConfig()` and cover:

```go
tests := []struct {
	name     string
	endpoint string
	allow    bool
	wantErr  string
}{
	{"default AWS endpoint", "", false, ""},
	{"HTTPS", "https://objects.example.test", false, ""},
	{"HTTP denied", "http://objects.example.test", false, "insecure"},
	{"HTTP opted in", "http://objects.example.test", true, ""},
	{"scheme less", "localhost:9000", true, "absolute HTTP or HTTPS"},
	{"FTP", "ftp://objects.example.test", true, "absolute HTTP or HTTPS"},
	{"missing host", "https:///bucket", true, "absolute HTTP or HTTPS"},
	{"malformed", "://bad", true, "absolute HTTP or HTTPS"},
}
```

Assert success or the stable error category, not AWS SDK behavior.

- [ ] **Step 2: Run the table test and observe RED**

Run:

```sh
go test ./packstore/s3store -run '^TestNewValidatesEndpointTransport$'
```

Expected: cleartext and invalid endpoints are accepted or fail later for
unrelated SDK parsing.

- [ ] **Step 3: Add endpoint validation**

Add the public config field:

```go
AllowInsecureTransport bool
```

and a private validator:

```go
func validateEndpoint(endpoint string, allowInsecure bool) error {
	if endpoint == "" {
		return nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("s3store: endpoint must be an absolute HTTP or HTTPS URL")
	}
	if parsed.Scheme == "http" && !allowInsecure {
		return fmt.Errorf("s3store: insecure HTTP endpoint requires AllowInsecureTransport")
	}
	return nil
}
```

Call it near the start of `New`, before `awsconfig.LoadDefaultConfig`.

- [ ] **Step 4: Opt the real-service test into local HTTP**

Set:

```go
AllowInsecureTransport: strings.HasPrefix(endpoint, "http://"),
```

in both conformance backend configurations so HTTPS conformance remains
secure by default and local MinIO remains explicit.

- [ ] **Step 5: Run focused tests and commit**

Run:

```sh
go test ./packstore/s3store
```

Then commit the endpoint policy change.

---

### Task 4: Resolution refresh and exact loose authority

**Files:**
- Modify: `packstore/location_resolver.go`
- Modify: `packstore/location_legacy.go`
- Modify: `packstore/location_resolver_test.go`
- Modify: `packstore/filesystem_backend.go`
- Modify: `packstore/filesystem_backend_test.go`

**Interfaces:**
- Consumes: the existing `retryResolution` state and legacy resolver adapter.
- Produces: one-shot refresh for `NewMultiStore` and an internal-only
  zero-location marker.

- [ ] **Step 1: Add failing migration-race test**

Introduce a resolver that returns an old missing location on its first call
and a new healthy location on its second. Open the hash and assert:

```go
assert.Equal(t, 2, resolver.calls)
assert.Equal(t, 1, oldBackend.opens)
assert.Equal(t, 1, newBackend.opens)
assert.Equal(t, content, got)
```

- [ ] **Step 2: Run the migration test and observe RED**

Run:

```sh
go test ./packstore -run '^TestMultiStoreRefreshesChangedResolutionAfterMissingLocation$'
```

Expected: `ErrPhysicalMissing` is returned after one resolver call.

- [ ] **Step 3: Enable the existing bounded retry**

In `NewMultiStore`, set:

```go
store.retryResolution = true
```

Do not alter the missing-only gate, `sameResolution`, or the one-retry bound.

- [ ] **Step 4: Convert ordinary resolver fixtures to explicit locations**

Replace multi-location `&LooseLocation{}` fixtures with exact raw locations:

```go
&LooseLocation{
	Encoding: LooseEncodingRaw,
	LogicalSize: int64(len(content)),
	StoredSize: int64(len(content)),
}
```

Keep zero-value construction only in tests specifically exercising the
legacy adapter or rejection behavior.

- [ ] **Step 5: Add failing sentinel rejection tests**

Assert `ReadLocation.Validate` rejects an unmarked `&LooseLocation{}` from an
ordinary resolver before opening a backend. Also call
`FilesystemBackend.OpenLoose` directly with `LooseLocation{}` and assert it
rejects the ambiguous authority. Retain a legacy `New` store read proving the
internal adapter still discovers the representation.

- [ ] **Step 6: Run sentinel tests and observe RED**

Run:

```sh
go test ./packstore -run 'TestMultiStoreRejectsAmbiguousLooseLocation|TestFilesystemBackendRejectsAmbiguousLooseLocation|TestLegacy'
```

Expected: ordinary and direct filesystem zero locations are still accepted.

- [ ] **Step 7: Add the private legacy marker**

Extend the location type:

```go
type LooseLocation struct {
	Encoding    LooseEncoding
	LogicalSize int64
	StoredSize  int64
	legacy      bool
}
```

Set `legacy: true` only in `legacyLocationResolver`. Require the marker for
encoding zero in `ReadLocation.Validate` and in every filesystem method that
uses legacy auto-discovery (`OpenLoose`, `OpenSeekableLoose`, and
`ReadLooseBounded`).

- [ ] **Step 8: Run focused tests and commit**

Run:

```sh
go test ./packstore
```

Then commit resolution refresh and exact loose authority together because
both change resolver admission.

---

### Task 5: Pre-publication pack-size enforcement

**Files:**
- Modify: `packstore/filesystem_backend.go`
- Modify: `packstore/filesystem_backend_test.go`
- Modify: `packstore/s3store/publication.go`
- Modify: `packstore/s3store/publication_test.go`

**Interfaces:**
- Consumes: configured `Limits.PackBytes`, caller `MaxBytes`, and optional
  known size.
- Produces: an effective maximum enforced before canonical publication.

- [ ] **Step 1: Add failing known-size admission tests**

For both backends, configure `PackBytes` below `ExpectedSize`, supply a reader
that records reads, and assert publication returns a container `LimitError`
without reading input. For S3, also assert no HTTP request starts a multipart
upload. For filesystem, assert the canonical pack path does not exist.

- [ ] **Step 2: Add failing streamed-overflow tests**

Pass `MaxBytes` greater than `PackBytes` and an unknown-size source exceeding
`PackBytes`. Assert filesystem creates no canonical path and S3 aborts its
multipart upload without calling complete.

- [ ] **Step 3: Run the size tests and observe RED**

Run:

```sh
go test ./packstore -run 'TestFilesystemBackendPublishPackRejectsConfiguredLimit'
go test ./packstore/s3store -run 'TestPublishPackRejectsConfiguredLimit'
```

Expected: filesystem honors the larger caller limit, while S3 completes or
attempts an oversized canonical publication.

- [ ] **Step 4: Normalize filesystem publication limits**

Before creating a generation or staging directory:

```go
maxBytes, err := effectivePackPublicationLimit(
	opts.MaxBytes, opts.ExpectedSize, opts.SizeKnown, b.limits.PackBytes,
)
if err != nil {
	return PackReceipt{}, err
}
```

The private helper rejects negative values, defaults zero to configured,
caps positive values with `min`, and returns a structured
`LimitPackContainerBytes` error when a known size exceeds the result. Pass
`maxBytes` to `copyBoundedContext`.

- [ ] **Step 5: Normalize S3 publication limits before multipart creation**

Apply the same semantics in `s3store` with a private helper and pass the
effective value to `multipartPublish`. Ensure overflow returns a structured
container `LimitError`; the existing deferred abort remains armed and
`CompleteMultipartUpload` is never called.

- [ ] **Step 6: Run focused tests and commit**

Run:

```sh
go test ./packstore ./packstore/s3store
```

Then commit the publication admission change.

---

### Task 6: Oversized S3 replica fallback

**Files:**
- Modify: `packstore/s3store/reader.go`
- Modify: `packstore/s3store/reader_test.go`

**Interfaces:**
- Consumes: container `LimitError` from `HeadObject.ContentLength`.
- Produces: candidate-specific physical-corrupt classification without
  changing logical blob-limit behavior.

- [ ] **Step 1: Strengthen the existing oversized-head test**

Require the error to preserve both identities:

```go
require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
var limit *packstore.LimitError
require.ErrorAs(t, err, &limit)
assert.Equal(t, packstore.LimitPackContainerBytes, limit.Dimension)
```

Add a multi-store integration seam with an oversized S3 candidate followed
by a healthy backend and assert the healthy content is returned.

- [ ] **Step 2: Add a logical-limit control**

Open a pack whose entry exceeds `BlobBytes` and assert the raw/stored/window
`LimitError` does not wrap `packstore.ErrPhysicalCorrupt` and does not fall
through to another candidate.

- [ ] **Step 3: Run tests and observe RED**

Run:

```sh
go test ./packstore/s3store -run 'TestDownloadPackRejectsOversizedContentLength|TestOversizedS3ReplicaFallsBack|TestS3LogicalBlobLimitDoesNotFallback'
```

Expected: the oversized container error is bare and resolution stops.

- [ ] **Step 4: Join only the container limit with corruption**

At the `HeadObject` admission check, return:

```go
limitErr := &packstore.LimitError{
	Dimension: packstore.LimitPackContainerBytes,
	Actual:    uint64(*head.ContentLength),
	Limit:     uint64(b.limits.PackBytes),
}
return nil, "", errors.Join(packstore.ErrPhysicalCorrupt, limitErr)
```

Do not change `mapPackLimit` for per-blob or decoder limits.

- [ ] **Step 5: Run focused tests and commit**

Run:

```sh
go test ./packstore/s3store
```

Then commit the replica fallback classification.

---

### Task 7: Final verification and PR update

**Files:**
- Modify only files required by failures found during verification.

**Interfaces:**
- Consumes: all preceding task commits.
- Produces: a pushed PR branch with local and CI evidence.

- [ ] **Step 1: Format and inspect**

Run `gofmt`/`goimports` on changed Go files, then:

```sh
git diff --check
git status --short
```

- [ ] **Step 2: Run local verification**

Run:

```sh
umask 027
go test ./...
go build ./...
go vet ./...
golangci-lint run ./packstore/... ./safefileio/...
go test -race ./pack ./packstore ./backup
```

- [ ] **Step 3: Run portability builds**

Compile the CI targets:

```sh
artifact_dir="$(mktemp -d)"
CGO_ENABLED=0 GOOS=linux GOARCH=386 go test -c -o "${artifact_dir}/pack-linux386.test" ./pack
CGO_ENABLED=0 GOOS=linux GOARCH=386 go test -c -o "${artifact_dir}/packstore-linux386.test" ./packstore
CGO_ENABLED=0 GOOS=linux GOARCH=386 go test -c -o "${artifact_dir}/backup-linux386.test" ./backup
CGO_ENABLED=0 GOOS=plan9 GOARCH=amd64 go test -c -o "${artifact_dir}/pack-plan9.test" ./pack
CGO_ENABLED=0 GOOS=plan9 GOARCH=amd64 go test -c -o "${artifact_dir}/packstore-plan9.test" ./packstore
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go test -c -o "${artifact_dir}/packstore-windows.test.exe" ./packstore
```

Remove the temporary directory after every command succeeds.

- [ ] **Step 4: Run real S3 conformance**

Start the same pinned S3-compatible service used by CI and run:

```sh
go test -race ./packstore/s3store -count=1
```

with the conformance environment configured for its HTTP loopback endpoint.

- [ ] **Step 5: Perform final review and push**

Inspect the complete diff and commit history, update the relevant kata issues
with truthful `work.*` metadata, then:

```sh
git push origin feature/multi-location-read-substrate
```

Monitor the replacement PR CI run through completion and report any remaining
uncommitted files or failing jobs.
