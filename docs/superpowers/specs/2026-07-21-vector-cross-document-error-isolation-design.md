# Classifier-Gated Cross-Document Encode Error Isolation

**Status:** Approved design

## Context

`vector.Fill` can pack chunks from multiple documents into one encoder call
when `FillOptions.Batch.BatchSize` is positive. This improves utilization for
corpora whose documents usually produce fewer chunks than the configured
encoder batch size.

A failed shared call does not, by itself, identify which document caused the
failure. The first cross-document implementation diagnoses every such failure
by re-encoding every document slice in the failed batch before the serialized
collector applies any result. That preserves per-document `OnEncodeError`
semantics, but it treats transport, authentication, rate-limit, configuration,
and service failures like potentially document-specific failures. One failed
request can therefore expand into one request per document in the batch before
the caller is allowed to abort. Callers whose encoders have their own retry and
backoff policies multiply this cost again.

The error path needs two decisions with different owners:

1. The caller knows whether an error category could plausibly be caused by an
   individual input and is therefore worth isolating.
2. Once a failure is attributed to one document, `OnEncodeError` decides
   whether that document may be stamped without vectors or whether the fill
   must abort.

Kit must preserve this separation. Classification permits diagnosis; it never
permits skipping.

## Goals

- Abort shared transport, authentication, rate-limit, configuration, and
  service failures without document-level request amplification.
- Preserve poison-document skipping for callers that explicitly classify an
  error as potentially input-specific.
- Diagnose shared failures incrementally on Fill's serialized collector path
  and stop after the first failed document whose hook rejects a skip.
- Invoke `OnEncodeError` at most once for each failed document.
- Attribute `InvalidVectorError` directly from its batch chunk index without
  re-encoding the known-invalid document slice.
- Preserve vector ordering, document boundaries, serialized saves, stale
  revision handling, cancellation, and provider error unwrapping.
- Keep the legacy per-document path unchanged when `BatchSize <= 0`.

## Non-goals

- Inferring HTTP or provider-specific retryability inside kit.
- Adding a blanket rule that an all-poison batch must abort. Multiple genuine
  poison documents may all be skip-stamped when the caller authorizes each
  skip.
- Changing vector storage, generation fingerprints, persisted state, or
  backend interfaces.
- Changing caller-specific repair loops that invoke `EncodeBatched` directly
  rather than using `Fill`.
- Requiring callers to add benign-content replay. Replay is optional caller
  hardening beyond the classifier contract.

## Public API

Add one optional field to `FillOptions`:

```go
type FillOptions[K comparable] struct {
    // Existing fields remain unchanged.

    // ShouldIsolateBatchError reports whether an error from a shared,
    // multi-document encode call might be caused by an individual input and
    // is worth diagnosing at document-slice granularity.
    //
    // Fill does not call this function for single-document batches or for
    // errors wrapping context.Canceled or context.DeadlineExceeded. Errors
    // carrying exact batch-position attribution (*InvalidVectorError) also
    // bypass this function and go directly to OnEncodeError. Returning true
    // permits diagnosis only; OnEncodeError still owns the decision to
    // skip-stamp an attributed document. Returning false, or leaving this nil,
    // aborts Fill without document-level retries.
    ShouldIsolateBatchError func(error) bool
}
```

The classifier runs on the serialized collector goroutine. Provider errors
remain wrapped with `%w`, so the classifier and the final Fill caller can use
`errors.As` and `errors.Is`.

An added exported struct field is source-compatible for keyed literals. An
unkeyed `FillOptions` literal must be updated when consuming the release.

## Terminology

A **document slice** is the contiguous refs belonging to one document inside
one failed encode batch. It is not necessarily the document's complete chunk
list: fixed-size batching can cut through a document, so the same document can
have slices in multiple batches. Diagnosis, unattributed-failure checks, and
recovery are scoped to the failed batch. Document state and the exactly-once
hook decision remain scoped to the whole document.

An **active document** has not already been saved and has not already received
a failed/skip decision from another batch result.

## Architecture

Workers perform only the initially scheduled encode calls. On failure, a worker
returns the original error and its refs; it does not perform document probes.
The collector serializes classification, incremental probing, hook decisions,
state updates, and saves.

This placement has three deliberate consequences:

- The hook stays serialized and needs no caller-side locking.
- The unbuffered result channel back-pressures workers while a failure is being
  diagnosed. Workers that have completed calls block on result delivery and do
  not take more jobs while the endpoint may be unhealthy.
- The collector can stop probing immediately when an exact document failure is
  not skippable, then return an error so `runFillJobs` cancels in-flight work.

Probes always use Fill's outer `ctx`, not the worker `workCtx`. The probe loop
checks `ctx` before each call and after it returns, and separately treats probe
errors wrapping `context.Canceled` or `context.DeadlineExceeded` as fatal even
when the outer context remains live. An encoder-internal timeout is never
presented to `OnEncodeError` as a document-specific failure.

## Document Decision State

Extend the collector-owned document state to represent an error decision
separately from raw encoder output. The exact field names are implementation
details, but the state must distinguish:

- no decision yet;
- a failed document whose hook rejected skipping;
- a pre-decided skip whose stamp is still pending;
- a saved document.

There is one collector helper that consults `OnEncodeError`. It records the
decision before any save attempt. A nil hook is equivalent to a hook returning
false. `saveEncoded` consumes the recorded skip decision and performs a
stamp-only save without consulting the hook again. This makes "exactly one hook
invocation per failed document" structural rather than dependent on control
flow.

If a skip-stamp save returns `ErrStale`, the document remains pending under the
existing stale semantics; the same Fill run does not consult the hook again.

## State Transitions

### Successful batch

Filter out refs for documents already saved or failed by another result,
scatter the remaining vectors into their exact document and chunk positions,
and save newly complete documents using the existing ordered or unordered save
policy.

### Context failure

If the initial shared call returns an error wrapping `context.Canceled` or
`context.DeadlineExceeded`, abort immediately. Do not call the classifier, the
hook, or a probe.

### Single-document batch failure

A batch whose refs all belong to one document already has exact attribution.
If another result has already decided or saved that document, discard this
late result without another hook call. Otherwise, translate any
`InvalidVectorError` from slice-relative to document-relative chunk position,
make the document's one hook decision, and either abort or record a skip. Do
not call the classifier or re-encode the slice.

### Shared batch with `InvalidVectorError`

`InvalidVectorError.Chunk` is the index within the chunk list passed to
`EncodeBatched`. Because the shared batch is encoded in one call,
`refs[invalid.Chunk]` identifies both the failing document and its
document-relative chunk.

1. Validate that `invalid.Chunk` indexes `refs`. An out-of-range index is a
   fatal internal-contract error and never falls through to provider
   classification.
2. Translate and wrap the error so `errors.As` still finds
   `*InvalidVectorError` and its `Chunk` is document-relative.
3. If another result has already decided or saved the attributed document, do
   not consult the hook again. Recover any other active slices and discard the
   now-obsolete invalid result.
4. Otherwise, consult `OnEncodeError` for the attributed document before
   issuing any recovery probes.
5. If the hook is nil or rejects skipping, abort with zero additional encoder
   calls.
6. If it accepts, record the pre-decided skip and re-encode only the other
   active document slices. The known-invalid slice is never re-encoded.

Each recovery re-encode follows the same per-probe context checks, wrapped
context-error handling, invalid-vector offset translation, and exactly-once
hook decision described below for diagnosis probes. A recovery failure is
never reclassified.

The other slices must be recovered because `EncodeBatched` returns no vector
result when validation fails. A nondeterministic encoder cannot erase the exact
attribution by succeeding on a retry because the failing slice is not retried.

### Other shared-batch failure

If `ShouldIsolateBatchError` is nil or returns false, abort immediately with the
original error wrapped in the existing document context. If it returns true,
filter the failed batch's document slices against current state, then probe the
remaining active slices sequentially in batch order.

For each probe:

1. Check the outer context.
2. Encode only that document's refs within this failed batch.
3. On success, scatter the recovered vectors.
4. On an error wrapping a context cancellation or deadline, abort without a
   hook call.
5. On `InvalidVectorError`, translate the slice-relative chunk by both the
   document-slice starting chunk and the invalid vector's local index.
6. For any other exact failure, preserve the provider error through wrapping.
7. Make the document's single hook decision. A nil or false hook aborts
   immediately; a true hook records a skip and diagnosis continues.

### Unattributed shared failure

If every active document slice succeeds in isolation and no document already
failed by another result explains the original error, retain the fatal
"shared batch failed but no document failed in isolation" outcome. This avoids
silently hiding request-shape or transport behavior that only occurs jointly.

If every possible cause of a late result is a document already failed by
another batch, recover any still-active neighbors and discard the obsolete
batch error. This interleaving is expected when multiple batches are in flight.

## Concurrency and Ordering Invariants

- Saves, probes, classification, and hook decisions occur only on the
  collector goroutine.
- A collector probe back-pressures result delivery and pauses new job uptake.
- Concurrent-path diagnosis filters saved and failed documents before probing.
- A hook-rejected probe failure returns promptly and cancels in-flight workers.
- Cancellation between document slices prevents the next probe.
- With one fill worker, the current bounded encode window and its saves finish
  before another window starts.
- With multiple fill workers, successful unrelated documents may still be
  saved as their results arrive.
- Each document receives at most one `OnEncodeError` invocation even when it
  spans batches or participates in multiple failed calls.

## Error Transparency

All new wrappers use `%w`. The following types must remain discoverable through
the complete Fill error chain:

- provider-specific errors used by callers for classification and backoff;
- `*InvalidVectorError` with a document-relative `Chunk`;
- `context.Canceled` and `context.DeadlineExceeded`.

The classifier receives the wrapped error produced by the initial
`EncodeBatched` call. It must be able to recover a provider-specific error with
`errors.As`. Fill's final returned error must preserve the same property.

## Test Matrix

### Classification and request counts

- A shared provider error with a nil classifier performs exactly one encode,
  no hook call, and aborts.
- A classifier-false shared error performs exactly one encode, one classifier
  call, no hook call, and aborts.
- A classifier-true failure whose first probe fails and whose hook is nil or
  false performs one shared encode plus one probe, then aborts.
- A classifier-true failure with an accepted poison document probes remaining
  slices, skip-stamps the poison document, and saves recovered neighbors.
- A batch containing only two poison documents invokes the hook once for each,
  skip-stamps both, and does not manufacture an all-failed fatal result.
- A request-shape error for which every active slice succeeds retains the
  unattributed fatal result and performs one shared call plus one call per
  active document slice.
- Single-document and context-failure paths never invoke the classifier.

### Invalid-vector attribution

- An attributed shared invalid vector with a nil or false hook performs only
  the shared call and aborts with the exact document and chunk.
- An accepted attributed invalid vector never re-encodes its failing slice and
  recovers only other active slices.
- An out-of-range invalid chunk is fatal, with no classifier or hook call.
- Offset translation is tested with the first probed slice: the document spans
  a batch boundary, the slice begins at document chunk greater than zero, and
  its probe returns an invalid vector at a slice-relative index greater than
  zero. The hook observes `slice start + local index`; total calls are one
  shared call plus one probe.
- A pre-decided skip reaches saving with exactly one hook invocation.

### Scheduling and state

- A late failed result filters documents already saved or skip-decided by
  another batch.
- A batch error explained only by an already-failed document is discarded after
  active neighbors are recovered.
- A document spanning two failed batches still receives at most one hook
  decision globally.
- A blocked collector probe prevents workers from taking additional jobs.
- A hook-rejected probe failure promptly cancels an observable in-flight
  worker.
- Probe cancellation and wrapped context errors stop further diagnosis.
- Ordered save failure behavior, stale handling, empty documents, stats, vector
  order, and concurrency bounds retain their existing tests.

### Error transparency

- A fake provider error remains `errors.As`-discoverable inside the classifier
  and in Fill's returned error.
- A probe error remains discoverable after document-context wrapping.
- Invalid-vector and context errors retain `errors.As`/`errors.Is` behavior.

## Caller Wiring

Callers that use positive `BatchSize` and rely on `OnEncodeError` must add a
classifier in the same dependency upgrade.

A caller with a body-aware permanent HTTP error type should classify statuses
that might represent input rejection, such as 400, 413, and 422. Its existing
body-aware hook remains the definitive skip decision. A benign-content replay
can further distinguish request-shape failures whose response bodies resemble
input-specific failures, but that is separable hardening rather than a kit
upgrade requirement.

A caller that treats HTTP 400 as ambiguous should classify only that status,
then retain its benign-shape replay inside `OnEncodeError`. Authentication,
rate-limit, service, and transport failures remain classifier-false and abort
after the shared call. Its returned Fill error must still unwrap to the
provider type used by reconciler backoff.

Caller-specific direct repair loops that do not use `Fill` need no changes.

## Rollout

1. Land the kit API, collector-path diagnosis, state changes, tests, and
   `vector/AGENTS.md` invariants.
2. Publish the change as a minor release because it adds public API and changes
   positive-`BatchSize` behavior. Release notes lead with the existing option's
   semantic change from per-document sub-batching to cross-document packing,
   which changes request shapes for every upgrading caller. They then call out
   the conservative nil-classifier default for shared failures.
3. In each consumer, combine the dependency bump and classifier wiring in one
   change. Consumers still pinned to an older kit release are unaffected, so
   repositories do not require an atomic simultaneous release.
4. Update caller documentation that describes fill concurrency as concurrent
   documents; cross-document packing makes concurrent encode batches or
   requests the accurate unit.
5. Keep optional benign-content replay hardening separate when a caller already
   has a conservative body-aware permanent-error check.

## Compatibility

- Keyed `FillOptions` literals continue to compile; unkeyed literals require an
  update.
- `BatchSize <= 0` retains legacy per-document behavior.
- Single-document batches retain direct hook behavior.
- Positive `BatchSize` plus a nil classifier fails closed for shared provider
  errors.
- Returning true from the classifier never authorizes a skip.
- Partial successful saves remain valid after an abort; pending documents retry
  normally.
- There are no database, vector-format, generation, or fingerprint changes.

## Verification Gates

- Run the complete kit suite and vector race tests.
- Test each consumer against the local kit checkout before publishing or
  upgrading.
- Assert failure-path request counts, not only returned errors.
- Verify provider errors survive the Fill-to-caller wrapping chain.
- Verify hook-false diagnosis cancels in-flight work promptly.
- Verify all-poison and joint-shape-failure batches retain distinct outcomes.

## Alternatives Considered

### Classifier with eager all-document isolation

This prevents amplification for classifier-false errors but still probes an
entire ambiguous batch before the hook can abort. It does not adequately bound
request-level 400 failures.

### Provider errors carrying rejected input indexes

Exact provider attribution would avoid probes, but common batch embedding APIs
do not report rejected indexes. Requiring it would move provider-specific
probing into every caller and would not cover existing endpoints.

### Abort when every isolated document fails

This could guard one class of request-level failures, but it overrides the
caller's skip decision and wedges batches containing multiple genuine poison
documents. Kit must consult the hook once per exact failure and respect the
result.
