# Vector Cross-Document Error Isolation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make cross-document `vector.Fill` batches fail closed for unclassified errors while preserving bounded, exactly-once document isolation for errors callers identify as potentially input-specific.

**Architecture:** Encode workers perform only their scheduled shared calls and return the untouched result to the serialized collector. The collector classifies unattributed shared errors, incrementally probes document slices, translates exact invalid-vector positions, records one hook decision per failed document, and applies saves; a result-delivery acknowledgement pauses the worker that reported a failure until collection completes. The legacy `BatchSize <= 0` path and direct single-document error behavior remain unchanged.

**Tech Stack:** Go 1.26.3, standard-library `context`, `errors`, `fmt`, and `sync`, plus `github.com/stretchr/testify/assert` and `github.com/stretchr/testify/require` for tests.

## Global Constraints

- Keep `go.kenn.io/kit/vector` provider-neutral; the classifier receives errors but does not interpret HTTP statuses or provider payloads.
- Preserve all error causes with `%w` so `errors.As` and `errors.Is` continue through classifier, hook, and caller boundaries.
- A document slice means only that document's contiguous refs inside one failed shared batch; it may be a partial document when a batch boundary cuts through it.
- Call `OnEncodeError` at most once per failed document. A rejected hook decision aborts immediately and is not stored as durable collector state.
- Never classify context errors, single-document failures, or errors carrying an in-range `*InvalidVectorError` batch position.
- A nil classifier fails closed for unattributed shared errors. Returning true permits diagnosis only; it never permits skipping.
- Probes and invalid-vector recovery run on the collector with Fill's outer `ctx`; probe failures are never reclassified.
- Retain the existing `BatchSize <= 0` per-document path, vector ordering, save serialization, stale handling, and statistics.
- Public docs and tests must remain caller-neutral and contain no private downstream project names.

## File Map

- Modify `vector/flow.go`: public classifier option, raw worker result, collector-owned diagnosis and decision state, direct invalid-vector recovery, and failed-result acknowledgement.
- Create `vector/flow_batch_error_test.go`: focused request-count, attribution, cancellation, concurrency, and error-transparency coverage.
- Create `vector/flow_internal_test.go`: batch-scoped collector tests that require prebuilt partial-document state.
- Modify `vector/flow_test.go`: opt existing isolation-dependent tests into classification and retain regression coverage for prior behavior.
- Modify `vector/AGENTS.md`: replace unconditional isolation guidance with the classifier-gated collector invariants.

---

### Task 1: Fail-Closed Classification and Collector-Owned Probing

**Files:**

- Modify: `vector/flow.go`
- Create: `vector/flow_batch_error_test.go`
- Modify: `vector/flow_test.go`

**Interfaces:**

- Consumes: existing `EncodeFunc`, `EncodeBatched`, `FillOptions`, `fillChunkRef`, `fillDocumentState`, `saveReadyDocuments`, and `offsetInvalidVectorError`.
- Produces: `FillOptions.ShouldIsolateBatchError func(error) bool`, `fillBatchResult.err error`, `fillEncoded.skip bool`, and collector helpers `decideFillDocumentError`, `probeFillDocumentSlices`, `applyFillVectors`, and `fillRefsContainFailedDocument`.

- [ ] **Step 1: Add focused tests for the classifier boundary and exact request counts**

Create `vector/flow_batch_error_test.go` with the provider error type and these tests. The first table proves nil and false classifiers stop after the shared request, do not consult the hook, and preserve the provider error for the classifier and final caller. The next table proves a true classifier issues only the first probe when that probe is rejected, including the nil-hook case. The final test proves two genuine poison documents can both be accepted without an all-failed backstop.

```go
package vector_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/vector"
)

type fillProviderError struct {
	code int
}

func (e *fillProviderError) Error() string {
	return fmt.Sprintf("provider status %d", e.code)
}

func TestFillSharedErrorClassifierFailsClosedWithoutProbes(t *testing.T) {
	providerErr := &fillProviderError{code: 503}
	for _, tc := range []struct {
		name            string
		classifier      func(error) bool
		wantClassifiers int
	}{
		{name: "nil classifier"},
		{name: "classifier false", wantClassifiers: 1, classifier: func(err error) bool {
			var got *fillProviderError
			require.ErrorAs(t, err, &got)
			return false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			store.content = map[int64]string{1: "one", 2: "two", 3: "three"}
			var calls, classifiers, hooks int
			enc := func(context.Context, []string) ([][]float32, error) {
				calls++
				return nil, providerErr
			}
			classifier := tc.classifier
			if classifier != nil {
				classifier = func(err error) bool {
					classifiers++
					return tc.classifier(err)
				}
			}
			_, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
				ScanBatch: 3,
				Batch:     vector.BatchOptions{BatchSize: 3},
				ShouldIsolateBatchError: classifier,
				OnEncodeError: func(int64, error) bool {
					hooks++
					return true
				},
			})
			require.Error(t, err)
			var got *fillProviderError
			require.ErrorAs(t, err, &got)
			assert.Same(t, providerErr, got)
			assert.Equal(t, 1, calls)
			assert.Equal(t, tc.wantClassifiers, classifiers)
			assert.Zero(t, hooks)
		})
	}
}

func TestFillSharedErrorRejectedFirstProbeStopsDiagnosis(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook func(int64, error) bool
	}{
		{name: "nil hook"},
		{name: "false hook", hook: func(int64, error) bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			store.content = map[int64]string{1: "poison one", 2: "poison two"}
			var calls, classifiers, hooks int
			enc := func(context.Context, []string) ([][]float32, error) {
				calls++
				return nil, &fillProviderError{code: 400}
			}
			hook := tc.hook
			if hook != nil {
				hook = func(doc int64, err error) bool {
					hooks++
					return tc.hook(doc, err)
				}
			}
			_, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
				ScanBatch: 2,
				Batch:     vector.BatchOptions{BatchSize: 2},
				ShouldIsolateBatchError: func(error) bool {
					classifiers++
					return true
				},
				OnEncodeError: hook,
			})
			require.Error(t, err)
			var providerErr *fillProviderError
			require.ErrorAs(t, err, &providerErr)
			assert.Equal(t, 2, calls, "one shared call plus the first probe")
			assert.Equal(t, 1, classifiers)
			assert.Equal(t, map[bool]int{true: 1, false: 0}[tc.hook != nil], hooks)
		})
	}
}

func TestFillSharedErrorAllowsTwoPoisonDocuments(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{1: "poison one", 2: "poison two"}
	var calls int
	hooks := map[int64]int{}
	enc := func(context.Context, []string) ([][]float32, error) {
		calls++
		return nil, &fillProviderError{code: 400}
	}
	stats, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
		ScanBatch:                  2,
		Batch:                      vector.BatchOptions{BatchSize: 2},
		ShouldIsolateBatchError: func(error) bool { return true },
		OnEncodeError: func(doc int64, _ error) bool {
			hooks[doc]++
			return true
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 3, calls)
	assert.Equal(t, map[int64]int{1: 1, 2: 1}, hooks)
	assert.Equal(t, 2, stats.Skipped)
	assert.Zero(t, stats.Documents)
}
```

- [ ] **Step 2: Run the new tests and verify the public field is missing**

Run:

```bash
go test ./vector -run 'TestFillSharedError(ClassifierFailsClosedWithoutProbes|RejectedFirstProbeStopsDiagnosis|AllowsTwoPoisonDocuments)$' -count=1
```

Expected: compilation fails because `vector.FillOptions[int64]` has no field or method named `ShouldIsolateBatchError`.

- [ ] **Step 3: Add the classifier API and make workers return only raw batch results**

In `vector/flow.go`, append the field below to `FillOptions` after `OnEncodeError`:

```go
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
```

Replace the two result structs and `encodeFillBatch` with raw-result state. `skip` is the structural signal that `saveEncoded` must not consult the hook again.

```go
type fillEncoded[K comparable] struct {
	doc     Pending[K]
	chunks  []Chunk
	vectors []Vector
	err     error
	skip    bool
}

type fillBatchResult struct {
	refs    []fillChunkRef
	vectors []Vector
	err     error
}

func encodeFillBatch(ctx context.Context, enc EncodeFunc, refs []fillChunkRef) fillBatchResult {
	chunks := make([]Chunk, len(refs))
	for i, ref := range refs {
		chunks[i] = ref.value
	}
	vectors, err := EncodeBatched(ctx, enc, chunks, BatchOptions{})
	return fillBatchResult{refs: refs, vectors: vectors, err: err}
}
```

- [ ] **Step 4: Add the single hook-decision and collector probe helpers**

Add these helpers after `offsetInvalidVectorError`. `decideFillDocumentError` stores only accepted skips; a nil or false hook returns immediately. `probeFillDocumentSlices` filters each slice against current state, uses the outer context, translates a slice-relative invalid-vector position, and saves after each decided slice. It returns whether any active slice failed, which distinguishes poison input from a joint request-shape failure.

```go
func applyFillVectors[K comparable](refs []fillChunkRef, vectors []Vector, states []fillDocumentState[K]) {
	for i, ref := range refs {
		state := &states[ref.doc]
		if state.saved || state.failed {
			continue
		}
		state.encoded.vectors[ref.chunk] = vectors[i]
		state.remaining--
	}
}

func fillRefsContainFailedDocument[K comparable](refs []fillChunkRef, states []fillDocumentState[K]) bool {
	for _, ref := range refs {
		if states[ref.doc].failed {
			return true
		}
	}
	return false
}

func splitFillRefsByDocument(refs []fillChunkRef) [][]fillChunkRef {
	var slices [][]fillChunkRef
	for start := 0; start < len(refs); {
		end := start + 1
		for end < len(refs) && refs[end].doc == refs[start].doc {
			end++
		}
		slices = append(slices, refs[start:end])
		start = end
	}
	return slices
}

func decideFillDocumentError[K comparable](
	ctx context.Context, o FillOptions[K], states []fillDocumentState[K], doc int, err error,
) error {
	state := &states[doc]
	if state.saved || state.failed {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("encode document %v: %w", state.encoded.doc.Doc, err)
	}
	if o.OnEncodeError == nil || !o.OnEncodeError(state.encoded.doc.Doc, err) {
		return fmt.Errorf("encode document %v: %w", state.encoded.doc.Doc, err)
	}
	state.encoded.err = err
	state.encoded.skip = true
	state.remaining = 0
	state.failed = true
	return nil
}

func probeFillDocumentSlices[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc, o FillOptions[K],
	refs []fillChunkRef, states []fillDocumentState[K], orderedSaves bool,
	stale map[K]struct{}, stats *FillStats,
) (bool, error) {
	failed := false
	for _, documentSlice := range splitFillRefsByDocument(refs) {
		active := activeFillRefs(documentSlice, states)
		if len(active) == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return failed, err
		}
		probe := encodeFillBatch(ctx, enc, active)
		if err := ctx.Err(); err != nil {
			return failed, err
		}
		if probe.err != nil {
			if errors.Is(probe.err, context.Canceled) || errors.Is(probe.err, context.DeadlineExceeded) {
				return failed, fmt.Errorf("encode document %v: %w",
					states[active[0].doc].encoded.doc.Doc, probe.err)
			}
			failed = true
			err := offsetInvalidVectorError(probe.err, active[0].chunk)
			if err := decideFillDocumentError(ctx, o, states, active[0].doc, err); err != nil {
				return failed, err
			}
		} else {
			applyFillVectors(active, probe.vectors, states)
		}
		if err := saveReadyDocuments(ctx, store, gen, o, states, orderedSaves, stale, stats); err != nil {
			return failed, err
		}
	}
	return failed, nil
}
```

- [ ] **Step 5: Move shared-error diagnosis into `applyFillBatch`**

Change both `applyFillBatch` call sites in `fillPageAcrossDocuments` to pass `enc` after `o`. Replace `applyFillBatch` with the implementation below. Task 2 will specialize the shared `InvalidVectorError` branch; until then, it bypasses classification but is diagnosed with the same bounded probe loop.

```go
func applyFillBatch[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, o FillOptions[K], enc EncodeFunc,
	batch fillBatchResult, states []fillDocumentState[K], orderedSaves bool,
	stale map[K]struct{}, stats *FillStats,
) error {
	if batch.err == nil {
		applyFillVectors(batch.refs, batch.vectors, states)
		return saveReadyDocuments(ctx, store, gen, o, states, orderedSaves, stale, stats)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(batch.err, context.Canceled) || errors.Is(batch.err, context.DeadlineExceeded) {
		return fillBatchContextError(batch.err, batch.refs, states)
	}

	if batch.refs[0].doc == batch.refs[len(batch.refs)-1].doc {
		active := activeFillRefs(batch.refs, states)
		if len(active) == 0 {
			return nil
		}
		err := offsetInvalidVectorError(batch.err, active[0].chunk)
		if err := decideFillDocumentError(ctx, o, states, active[0].doc, err); err != nil {
			return err
		}
		return saveReadyDocuments(ctx, store, gen, o, states, orderedSaves, stale, stats)
	}

	explained := fillRefsContainFailedDocument(batch.refs, states)
	active := activeFillRefs(batch.refs, states)
	if len(active) == 0 {
		return nil
	}
	var invalid *InvalidVectorError
	if !errors.As(batch.err, &invalid) {
		if o.ShouldIsolateBatchError == nil || !o.ShouldIsolateBatchError(batch.err) {
			return fillBatchContextError(batch.err, active, states)
		}
	}
	failed, err := probeFillDocumentSlices(ctx, store, gen, enc, o, active, states,
		orderedSaves, stale, stats)
	if err != nil {
		return err
	}
	if !failed && !explained {
		return fillBatchContextError(fmt.Errorf(
			"cross-document batch failed but no document failed in isolation: %w", batch.err),
			active, states)
	}
	return nil
}

func fillBatchContextError[K comparable](
	err error, refs []fillChunkRef, states []fillDocumentState[K],
) error {
	for _, ref := range refs {
		if !states[ref.doc].saved {
			return fmt.Errorf("encode batch beginning with document %v: %w",
				states[ref.doc].encoded.doc.Doc, err)
		}
	}
	return err
}
```

Delete the old `docErrors`/`fatal` handling. Update `saveEncoded` so a collector-approved skip never calls `OnEncodeError` a second time:

```go
	skipped := r.skip
	if r.err != nil && !skipped {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(r.err, context.Canceled) || errors.Is(r.err, context.DeadlineExceeded) {
			return fmt.Errorf("encode document %v: %w", r.doc.Doc, r.err)
		}
		if o.OnEncodeError == nil || !o.OnEncodeError(r.doc.Doc, r.err) {
			return fmt.Errorf("encode document %v: %w", r.doc.Doc, r.err)
		}
		skipped = true
	}
```

- [ ] **Step 6: Opt existing isolation tests into the new contract**

In `vector/flow_test.go`, add the following field to `TestFillCrossDocumentBatchingIsolatesPoisonDocument` and `TestFillCrossDocumentBatchingConcurrentFailureKeepsAttribution`:

```go
		ShouldIsolateBatchError: func(error) bool { return true },
```

Add the same field to `TestFillCrossDocumentBatchingAbortsUnattributedBatchError`; its request-shape error must still probe every active slice before producing the existing unattributed fatal error. Do not add a classifier to single-document or cancellation tests.

- [ ] **Step 7: Run the focused and package tests**

Run:

```bash
gofmt -w vector/flow.go vector/flow_test.go vector/flow_batch_error_test.go
go test ./vector -run 'TestFill(SharedError|CrossDocumentBatching(IsolatesPoisonDocument|AbortsUnattributedBatchError|ConcurrentFailureKeepsAttribution))' -count=1
go test ./vector -count=1
```

Expected: both commands pass. The focused tests prove one-call fail-closed behavior, one-probe hook rejection, all-poison acceptance, and provider error transparency.

- [ ] **Step 8: Commit the collector classification slice**

Before committing, invoke the required `kenn:commit` skill. Then run:

```bash
git add vector/flow.go vector/flow_test.go vector/flow_batch_error_test.go
git commit -m "Bound shared batch error isolation"
```

Expected: one commit containing the public classifier and collector-owned generic diagnosis.

---

### Task 2: Direct Invalid-Vector Attribution and Recovery

**Files:**

- Modify: `vector/flow.go`
- Modify: `vector/flow_batch_error_test.go`
- Create: `vector/flow_internal_test.go`

**Interfaces:**

- Consumes: Task 1's `decideFillDocumentError`, `probeFillDocumentSlices`, `activeFillRefs`, and `fillBatchContextError`.
- Produces: `applyAttributedInvalidFillBatch`, which validates the batch position, translates it to the document chunk, decides the attributed document before recovery, never retries the known-invalid slice, and applies Task 1's per-probe rules to every recovery slice.

- [ ] **Step 1: Add direct-attribution request-count and recovery tests**

Append these tests to `vector/flow_batch_error_test.go`:

```go
func TestFillSharedInvalidVectorRejectedWithoutProbe(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{1: "good", 2: "bad", 3: "later"}
	var calls, classifiers, hooks int
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		calls++
		return [][]float32{{1}, {0}, {1}}, nil
	}
	_, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
		ScanBatch: 3,
		Batch:     vector.BatchOptions{BatchSize: 3},
		ShouldIsolateBatchError: func(error) bool {
			classifiers++
			return true
		},
		OnEncodeError: func(doc int64, err error) bool {
			hooks++
			assert.Equal(t, int64(2), doc)
			var invalid *vector.InvalidVectorError
			require.ErrorAs(t, err, &invalid)
			assert.Equal(t, 0, invalid.Chunk)
			return false
		},
	})
	require.Error(t, err)
	assert.Equal(t, 1, calls)
	assert.Zero(t, classifiers)
	assert.Equal(t, 1, hooks)
}

func TestFillSharedInvalidVectorNilHookRejectsWithoutProbe(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{1: "good", 2: "bad"}
	var calls, classifiers int
	enc := func(context.Context, []string) ([][]float32, error) {
		calls++
		return [][]float32{{1}, {0}}, nil
	}
	_, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
		ScanBatch: 2,
		Batch:     vector.BatchOptions{BatchSize: 2},
		ShouldIsolateBatchError: func(error) bool { classifiers++; return true },
	})
	require.Error(t, err)
	assert.Equal(t, 1, calls)
	assert.Zero(t, classifiers)
}

func TestFillSharedInvalidVectorRecoversOnlyOtherSlices(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{1: "good", 2: "bad", 3: "later"}
	var calls [][]string
	hooks := map[int64]int{}
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		calls = append(calls, append([]string(nil), texts...))
		out := make([][]float32, len(texts))
		for i, text := range texts {
			out[i] = []float32{1}
			if text == "bad" {
				out[i] = []float32{0}
			}
		}
		return out, nil
	}
	stats, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
		ScanBatch: 3,
		Batch:     vector.BatchOptions{BatchSize: 3},
		OnEncodeError: func(doc int64, _ error) bool {
			hooks[doc]++
			return true
		},
	})
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"good", "bad", "later"}, {"good"}, {"later"}}, calls)
	assert.Equal(t, map[int64]int{2: 1}, hooks)
	assert.Equal(t, 2, stats.Documents)
	assert.Equal(t, 1, stats.Skipped)
}

func TestFillSharedInvalidRecoveryFailureUsesProbeRules(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{1: "bad", 2: "neighbor"}
	var calls, classifiers int
	hooks := map[int64]int{}
	enc := func(context.Context, []string) ([][]float32, error) {
		calls++
		if calls == 1 {
			return [][]float32{{0}, {1}}, nil
		}
		return nil, &fillProviderError{code: 400}
	}
	_, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
		ScanBatch: 2,
		Batch:     vector.BatchOptions{BatchSize: 2},
		ShouldIsolateBatchError: func(error) bool { classifiers++; return true },
		OnEncodeError: func(doc int64, _ error) bool {
			hooks[doc]++
			return doc == 1
		},
	})
	require.Error(t, err)
	var providerErr *fillProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, 2, calls)
	assert.Zero(t, classifiers, "recovery failures are never reclassified")
	assert.Equal(t, map[int64]int{1: 1, 2: 1}, hooks)
}

func TestFillSharedInvalidVectorOutOfRangeIsFatal(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{1: "one", 2: "two"}
	var calls, classifiers, hooks int
	enc := func(context.Context, []string) ([][]float32, error) {
		calls++
		return nil, &vector.InvalidVectorError{Chunk: 2, Component: -1, Reason: "zero norm"}
	}
	_, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
		ScanBatch: 2,
		Batch:     vector.BatchOptions{BatchSize: 2},
		ShouldIsolateBatchError: func(error) bool { classifiers++; return true },
		OnEncodeError: func(int64, error) bool { hooks++; return true },
	})
	require.ErrorContains(t, err, "invalid vector chunk 2 outside batch of 2 chunks")
	var invalid *vector.InvalidVectorError
	require.ErrorAs(t, err, &invalid)
	assert.Equal(t, 1, calls)
	assert.Zero(t, classifiers)
	assert.Zero(t, hooks)
}

```

Create `vector/flow_internal_test.go` for the offset-composition test. Prebuilding the first document with chunks 0–2 already applied models a batch boundary without issuing an unrelated setup encode, so the counter measures exactly one failed shared call plus one probe.

```go
package vector

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type internalFillProviderError struct{}

func (*internalFillProviderError) Error() string { return "input rejected" }

type noOpFillStore struct{}

func (noOpFillStore) PendingForGeneration(context.Context, int, int) ([]Pending[int64], error) {
	return nil, nil
}

func (noOpFillStore) SaveVectors(context.Context, int, int64, any, []ChunkVector) error {
	return nil
}

func (noOpFillStore) LiveGenerations(context.Context) ([]int, error) { return nil, nil }

func (noOpFillStore) QueryGeneration(context.Context, int, Vector, int) ([]Hit[int64], error) {
	return nil, nil
}

func TestApplyFillBatchProbeInvalidVectorAddsSliceAndLocalOffsets(t *testing.T) {
	refs := []fillChunkRef{
		{doc: 0, chunk: 3, value: Chunk{Index: 3, Text: "d"}},
		{doc: 0, chunk: 4, value: Chunk{Index: 4, Text: "e"}},
		{doc: 1, chunk: 0, value: Chunk{Index: 0, Text: "z"}},
	}
	states := []fillDocumentState[int64]{
		{
			encoded: fillEncoded[int64]{
				doc: Pending[int64]{Doc: 10},
				chunks: []Chunk{{Index: 0}, {Index: 1}, {Index: 2}, {Index: 3}, {Index: 4}},
				vectors: []Vector{{1}, {1}, {1}, nil, nil},
			},
			remaining: 2,
		},
		{
			encoded: fillEncoded[int64]{
				doc: Pending[int64]{Doc: 20}, chunks: []Chunk{{Index: 0}}, vectors: make([]Vector, 1),
			},
			remaining: 1,
		},
	}
	var calls int
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		calls++
		if calls == 1 {
			assert.Equal(t, []string{"d", "e", "z"}, texts)
			return nil, &internalFillProviderError{}
		}
		assert.Equal(t, []string{"d", "e"}, texts)
		return [][]float32{{1}, {0}}, nil
	}
	batch := encodeFillBatch(context.Background(), enc, refs)
	var got *InvalidVectorError
	err := applyFillBatch(context.Background(), noOpFillStore{}, 7,
		FillOptions[int64]{
			ShouldIsolateBatchError: func(err error) bool {
				var providerErr *internalFillProviderError
				require.ErrorAs(t, err, &providerErr)
				return true
			},
			OnEncodeError: func(doc int64, err error) bool {
				assert.Equal(t, int64(10), doc)
				require.ErrorAs(t, err, &got)
				return false
			},
		},
		enc, batch, states, true, map[int64]struct{}{}, &FillStats{})
	require.Error(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 4, got.Chunk, "slice start 3 plus local invalid index 1")
	assert.Equal(t, 2, calls)
	assert.True(t, errors.As(err, &got))
}
```

- [ ] **Step 2: Run the direct-attribution tests and observe excess probing**

Run:

```bash
go test ./vector -run 'Test(FillSharedInvalidVector|ApplyFillBatchProbeInvalidVector)' -count=1
```

Expected: the rejected and recovery tests fail because Task 1 still probes the known-invalid document, and the out-of-range contract test lacks its fatal diagnostic.

- [ ] **Step 3: Implement exact invalid-vector attribution before generic classification**

Add this helper after `probeFillDocumentSlices`:

```go
func applyAttributedInvalidFillBatch[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc, o FillOptions[K],
	batch fillBatchResult, invalid *InvalidVectorError, states []fillDocumentState[K],
	orderedSaves bool, stale map[K]struct{}, stats *FillStats,
) error {
	if invalid.Chunk < 0 || invalid.Chunk >= len(batch.refs) {
		return fillBatchContextError(fmt.Errorf(
			"invalid vector chunk %d outside batch of %d chunks: %w",
			invalid.Chunk, len(batch.refs), batch.err), batch.refs, states)
	}

	failingRef := batch.refs[invalid.Chunk]
	failingState := &states[failingRef.doc]
	if !failingState.saved && !failingState.failed {
		err := offsetInvalidVectorError(batch.err, failingRef.chunk-invalid.Chunk)
		if err := decideFillDocumentError(ctx, o, states, failingRef.doc, err); err != nil {
			return err
		}
		if err := saveReadyDocuments(ctx, store, gen, o, states, orderedSaves, stale, stats); err != nil {
			return err
		}
	}

	recovery := make([]fillChunkRef, 0, len(batch.refs))
	for _, ref := range batch.refs {
		if ref.doc != failingRef.doc && !states[ref.doc].saved && !states[ref.doc].failed {
			recovery = append(recovery, ref)
		}
	}
	_, err := probeFillDocumentSlices(ctx, store, gen, enc, o, recovery, states,
		orderedSaves, stale, stats)
	return err
}
```

The offset is `failingRef.chunk - invalid.Chunk`: `offsetInvalidVectorError` adds that value to the batch-local index and therefore produces `failingRef.chunk`. This preserves the original wrapped `*InvalidVectorError` while replacing only its copied `Chunk` field.

In `applyFillBatch`, immediately after the context-error check and before active-ref filtering or classification, add:

```go
	var invalid *InvalidVectorError
	if errors.As(batch.err, &invalid) {
		return applyAttributedInvalidFillBatch(ctx, store, gen, enc, o, batch, invalid,
			states, orderedSaves, stale, stats)
	}
```

Then remove the temporary `InvalidVectorError` bypass around the classifier so the generic shared branch reads:

```go
	if o.ShouldIsolateBatchError == nil || !o.ShouldIsolateBatchError(batch.err) {
		return fillBatchContextError(batch.err, active, states)
	}
```

- [ ] **Step 4: Run direct attribution, legacy translation, and package tests**

Run:

```bash
gofmt -w vector/flow.go vector/flow_batch_error_test.go vector/flow_internal_test.go
go test ./vector -run 'Test(FillCrossDocumentBatchingTranslatesInvalidVectorChunkIndex|FillSharedInvalidVector|ApplyFillBatchProbeInvalidVector)' -count=1
go test ./vector -count=1
```

Expected: all tests pass. In particular, hook rejection performs one encode call, accepted attribution never includes `"bad"` in recovery calls, out-of-range attribution calls neither classifier nor hook, and the composed offset is document chunk 4.

- [ ] **Step 5: Commit direct attribution**

Before committing, invoke the required `kenn:commit` skill. Then run:

```bash
git add vector/flow.go vector/flow_batch_error_test.go vector/flow_internal_test.go
git commit -m "Attribute invalid shared vectors directly"
```

Expected: one commit isolating direct invalid-vector handling from generic provider-error diagnosis.

---

### Task 3: Failed-Result Backpressure, Cancellation, and Late Results

**Files:**

- Modify: `vector/flow.go`
- Modify: `vector/flow_batch_error_test.go`

**Interfaces:**

- Consumes: Task 1's raw `fillBatchResult.err` and collector probe path.
- Produces: `runFillJobs(..., holdUntilCollected func(R) bool, collect func(R) error) error`; failed shared results use an acknowledgement so their workers cannot take another job during collector diagnosis, while successful-result scheduling remains unchanged.

- [ ] **Step 1: Add observable backpressure and cancellation coverage**

Add `"sync/atomic"` and `"time"` to the imports in `vector/flow_batch_error_test.go`, then append:

```go
func TestFillRejectedProbeBackpressuresAndCancelsWorkers(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{
		1: "one", 2: "two", 3: "three", 4: "four", 5: "five", 6: "six",
	}
	secondStarted := make(chan struct{})
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	secondCanceled := make(chan struct{})
	var thirdStarted atomic.Bool
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		switch fmt.Sprint(texts) {
		case "[one two]":
			<-secondStarted
			return nil, &fillProviderError{code: 400}
		case "[three four]":
			close(secondStarted)
			<-ctx.Done()
			close(secondCanceled)
			return nil, ctx.Err()
		case "[one]":
			close(probeStarted)
			<-releaseProbe
			return nil, &fillProviderError{code: 400}
		case "[five six]":
			thirdStarted.Store(true)
			return [][]float32{{1}, {1}}, nil
		default:
			return nil, fmt.Errorf("unexpected texts %q", texts)
		}
	}

	done := make(chan error, 1)
	go func() {
		_, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
			ScanBatch:                  6,
			Batch:                      vector.BatchOptions{BatchSize: 2},
			Concurrency:                2,
			ShouldIsolateBatchError: func(error) bool { return true },
			OnEncodeError:              func(int64, error) bool { return false },
		})
		done <- err
	}()

	select {
	case <-probeStarted:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "collector did not start the first probe")
	}
	assert.False(t, thirdStarted.Load(), "the failed-result worker must wait for collection")
	close(releaseProbe)
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "Fill did not abort after hook rejection")
	}
	select {
	case <-secondCanceled:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "in-flight worker did not observe cancellation")
	}
	assert.False(t, thirdStarted.Load())
}
```

- [ ] **Step 2: Add late-result filtering and wrapped probe-context coverage**

Append two more tests. The first makes a one-document batch skip document 1 before a concurrently running shared batch containing document 1 returns; the shared result must filter document 1, recover document 2, and avoid a second hook call. The second proves an encoder-internal wrapped deadline is fatal even though Fill's outer context remains live.

```go
func TestFillLateSharedFailureFiltersDecidedDocument(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{1: "abc", 2: "d"}
	releaseShared := make(chan struct{})
	var hookCalls, classifierCalls atomic.Int32
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		switch fmt.Sprint(texts) {
		case "[a b]":
			return nil, &fillProviderError{code: 400}
		case "[c d]":
			<-releaseShared
			return nil, &fillProviderError{code: 400}
		case "[d]":
			return [][]float32{{1}}, nil
		default:
			return nil, fmt.Errorf("unexpected texts %q", texts)
		}
	}
	stats, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
		ScanBatch:   2,
		Split:       vector.SplitOptions{MaxRunes: 1},
		Batch:       vector.BatchOptions{BatchSize: 2},
		Concurrency: 2,
		ShouldIsolateBatchError: func(error) bool {
			classifierCalls.Add(1)
			return true
		},
		OnEncodeError: func(doc int64, _ error) bool {
			hookCalls.Add(1)
			assert.Equal(t, int64(1), doc)
			close(releaseShared)
			return true
		},
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), hookCalls.Load())
	assert.Equal(t, int32(1), classifierCalls.Load())
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, 1, stats.Documents)
	assert.True(t, store.embedded[2][7])
}

func TestFillWrappedProbeDeadlineAbortsWithoutHook(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{1: "one", 2: "two"}
	var calls, hooks int
	enc := func(context.Context, []string) ([][]float32, error) {
		calls++
		if calls == 1 {
			return nil, &fillProviderError{code: 400}
		}
		return nil, fmt.Errorf("encoder timeout: %w", context.DeadlineExceeded)
	}
	_, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
		ScanBatch:                  2,
		Batch:                      vector.BatchOptions{BatchSize: 2},
		ShouldIsolateBatchError: func(error) bool { return true },
		OnEncodeError: func(int64, error) bool { hooks++; return true },
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 2, calls)
	assert.Zero(t, hooks)
}
```

- [ ] **Step 3: Run the scheduling tests and verify the failed worker can advance**

Run:

```bash
go test ./vector -run 'TestFill(RejectedProbeBackpressuresAndCancelsWorkers|LateSharedFailureFiltersDecidedDocument|WrappedProbeDeadlineAbortsWithoutHook)$' -count=10
```

Expected before the acknowledgement change: `TestFillRejectedProbeBackpressuresAndCancelsWorkers` can observe `[five six]` starting while the collector is blocked in `[one]`. The other two tests pass only when concurrent filtering and wrapped context handling are correct.

- [ ] **Step 4: Add an optional collection acknowledgement to `runFillJobs`**

Replace `runFillJobs` with this signature and parallel result envelope. A nil predicate preserves the legacy per-document path. In the sequential branch there is no worker to hold, so collection remains direct.

```go
type fillJobResult[R any] struct {
	value     R
	collected chan struct{}
}

func runFillJobs[J, R any](
	ctx context.Context,
	workers int,
	jobs []J,
	work func(context.Context, J) R,
	holdUntilCollected func(R) bool,
	collect func(R) error,
) error {
	workers = min(workers, len(jobs))
	if workers <= 1 {
		for _, job := range jobs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := collect(work(ctx, job)); err != nil {
				return err
			}
		}
		return nil
	}

	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobCh := make(chan J)
	results := make(chan fillJobResult[R])
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for job := range jobCh {
				value := work(workCtx, job)
				var collected chan struct{}
				if holdUntilCollected != nil && holdUntilCollected(value) {
					collected = make(chan struct{})
				}
				select {
				case results <- fillJobResult[R]{value: value, collected: collected}:
				case <-workCtx.Done():
					return
				}
				if collected != nil {
					select {
					case <-collected:
					case <-workCtx.Done():
						return
					}
				}
			}
		})
	}
	go func() {
		defer close(jobCh)
		for _, job := range jobs {
			select {
			case jobCh <- job:
			case <-workCtx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	var firstErr error
	for result := range results {
		if firstErr == nil {
			if err := collect(result.value); err != nil {
				firstErr = err
				cancel()
			}
		}
		if result.collected != nil {
			close(result.collected)
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
```

- [ ] **Step 5: Wire failed shared results to the acknowledgement**

At both cross-document `runFillJobs` call sites, insert this predicate between `encode` and `collect`:

```go
			func(result fillBatchResult) bool { return result.err != nil },
```

At the legacy `fillPageByDocument` call site, insert `nil` between the work and collect functions:

```go
		nil,
```

Do not hold successful shared results: existing tests require an unrelated completed batch to reach the collector and save while another encode is slow.

- [ ] **Step 6: Run scheduling stress tests and the race detector**

Run:

```bash
gofmt -w vector/flow.go vector/flow_batch_error_test.go
go test ./vector -run 'TestFill(RejectedProbeBackpressuresAndCancelsWorkers|LateSharedFailureFiltersDecidedDocument|WrappedProbeDeadlineAbortsWithoutHook|CrossDocumentBatchingDoesNotBlockCompletedSaves)$' -count=20
go test -race ./vector -run 'TestFill(RejectedProbeBackpressuresAndCancelsWorkers|LateSharedFailureFiltersDecidedDocument|CrossDocumentBatchingConcurrentFailureKeepsAttribution)$' -count=1
go test ./vector -count=1
```

Expected: every run passes; the failed-result worker never starts the third batch, hook rejection cancels the second in-flight batch, successful-result saves remain prompt, and the race detector reports no races.

- [ ] **Step 7: Commit scheduling behavior**

Before committing, invoke the required `kenn:commit` skill. Then run:

```bash
git add vector/flow.go vector/flow_batch_error_test.go
git commit -m "Backpressure failed fill batches during diagnosis"
```

Expected: one commit containing the generic scheduler acknowledgement and its observable Fill regression tests.

---

### Task 4: Invariants, Compatibility Documentation, and Full Verification

**Files:**

- Modify: `vector/flow.go`
- Modify: `vector/AGENTS.md`
- Modify: `vector/flow_batch_error_test.go`

**Interfaces:**

- Consumes: all behavior implemented in Tasks 1–3.
- Produces: stable package documentation and durable contributor invariants for classifier-gated collector diagnosis, exactly-once hooks, outer-context probes, and failure-path backpressure.

- [ ] **Step 1: Add the remaining contract tests for classifier exclusions and exact-once decisions**

Append this table to `vector/flow_batch_error_test.go`. It pins that single-document and initial context errors bypass classification, while a pre-decided skip reaches `SaveVectors` without a second hook call.

```go
func TestFillBatchClassifierExclusions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		content   map[int64]string
		batchSize int
		encodeErr error
		wantHook  int
	}{
		{
			name:      "single document",
			content:   map[int64]string{1: "one"},
			batchSize: 2,
			encodeErr: &fillProviderError{code: 400},
			wantHook:  1,
		},
		{
			name:      "shared cancellation",
			content:   map[int64]string{1: "one", 2: "two"},
			batchSize: 2,
			encodeErr: fmt.Errorf("stopped: %w", context.Canceled),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			store.content = tc.content
			var classifiers, hooks int
			_, err := vector.Fill(context.Background(), store, 7,
				func(context.Context, []string) ([][]float32, error) { return nil, tc.encodeErr },
				vector.FillOptions[int64]{
					ScanBatch: len(tc.content),
					Batch:     vector.BatchOptions{BatchSize: tc.batchSize},
					ShouldIsolateBatchError: func(error) bool { classifiers++; return true },
					OnEncodeError: func(int64, error) bool { hooks++; return true },
				})
			if errors.Is(tc.encodeErr, context.Canceled) {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
			}
			assert.Zero(t, classifiers)
			assert.Equal(t, tc.wantHook, hooks)
		})
	}
}
```

Add `"errors"` to the test imports for this table.

- [ ] **Step 2: Rewrite the Fill package documentation around the shipped behavior**

Replace the final paragraph of the `Fill` doc comment in `vector/flow.go` with:

```go
// When Batch.BatchSize is positive, chunks from adjacent documents in one
// scan page may share an encode call. Errors with exact document attribution
// go directly to OnEncodeError. Other shared-call errors are diagnosed at
// document-slice granularity only when ShouldIsolateBatchError permits it;
// the nil default aborts without document-level retries. OnEncodeError remains
// the sole authority for skip-stamping an attributed document.
```

Keep the `Batch` and `Concurrency` field comments accurate about cross-document request packing and concurrent encode calls. Do not describe concurrency as parallel documents.

- [ ] **Step 3: Replace the unconditional isolation invariant in `vector/AGENTS.md`**

Replace the current shared-batch failure bullet under “Fill batches without losing document boundaries” with these exact bullets:

```markdown
- A failed shared encode batch is isolated only when
  `ShouldIsolateBatchError` permits document-slice diagnosis. A nil or false
  classifier aborts without probes; classification never authorizes a skip.
- A document slice is one document's refs within the failed batch, not
  necessarily its complete chunk list. Probes, classification, hook decisions,
  and saves stay on the serialized collector and use Fill's outer context.
- Errors carrying an in-range `*InvalidVectorError` batch index bypass
  classification. Decide the attributed document before recovering other
  slices, never retry the known-invalid slice, and make an out-of-range index
  fatal.
- Consult `OnEncodeError` exactly once per failed document. Record an accepted
  skip before saving so `saveEncoded` does not consult the hook again; a nil or
  rejected hook aborts immediately.
- Collector diagnosis back-pressures the worker that delivered the failed
  result. Filter concurrently completed results against documents already
  saved or skip-decided, and cancel in-flight workers promptly on abort.
- If every active slice succeeds in isolation and no already-failed document
  explains the shared error, keep the original error fatal. Preserve provider,
  invalid-vector, and context causes through all wrappers with `%w`.
```

- [ ] **Step 4: Run formatting, focused repetition, race, vet, and the complete suite**

Run:

```bash
gofmt -w vector/flow.go vector/flow_test.go vector/flow_batch_error_test.go
go test ./vector -run 'TestFill(Shared|Probe|Rejected|Late|Wrapped|BatchClassifier|CrossDocument)' -count=20
go test -race ./vector -count=1
go vet ./...
go test ./...
```

Expected: all four commands exit 0. The repeated suite has stable request counts and channel ordering; the race detector is clean; vet and the repository-wide tests report no failures.

- [ ] **Step 5: Review compatibility and public-content gates**

Inspect the diff:

```bash
git diff --check
git diff -- vector/flow.go vector/flow_test.go vector/flow_batch_error_test.go vector/AGENTS.md
```

Expected: `git diff --check` is silent and the implementation diff shows no provider-specific policy or caller-specific names in code, tests, or package docs.

Confirm the release and caller gates in the handoff, without changing another repository in this plan:

- Release notes must lead with the semantic change that positive `BatchSize` now packs chunks across documents and therefore changes request shapes; the conservative nil-classifier behavior is the second item.
- Every caller that sets positive `BatchSize` and relies on poison-document skipping must wire `ShouldIsolateBatchError` in the same dependency-upgrade change.
- Caller classifiers should identify errors that might be input-specific; existing `OnEncodeError` logic remains the definitive skip check.
- Provider errors returned by Fill must remain `errors.As`-discoverable for caller backoff logic.
- Direct repair loops that do not call `vector.Fill` require no change.
- Benign-content replay is separable hardening and does not gate the kit upgrade when a caller already has a conservative body-aware hook.

- [ ] **Step 6: Commit documentation and final test coverage**

Before committing, invoke `kenn:scrub-private-data` because this public branch is leaving local context, then invoke the required `kenn:commit` skill. Run:

```bash
git add vector/flow.go vector/flow_batch_error_test.go vector/AGENTS.md
git commit -m "Document fill error isolation invariants"
```

Expected: the final implementation commit contains only public docs, invariant updates, and the remaining contract tests. `git status --short` is empty afterward.

## Execution Checkpoints

After each task, review the staged or committed diff against these non-negotiable outcomes:

1. Classifier false or nil: one shared call, zero probes, zero hooks.
2. Classifier true plus rejected first probe: one shared call, one probe, one hook at most.
3. Direct shared invalid vector: zero classifier calls and hook decision before any recovery.
4. Accepted skip: hook state is consumed by saving without a second invocation.
5. Probe failure: outer-context checks, wrapped context fatality, invalid-vector offset translation, and no reclassification.
6. Late concurrent result: saved or skip-decided refs are filtered before recovery.
7. All-poison input: each exact failure may be accepted; no synthetic all-failed fatal.
8. Joint request-shape failure: all successful probes still leave the original error fatal unless an already-failed document explains it.
9. Every new wrapper uses `%w`, and provider errors remain `errors.As`-discoverable at the classifier and returned Fill error.
10. Positive `BatchSize` release notes and caller classifier wiring are treated as rollout requirements, while direct repair loops and optional benign replay stay out of implementation scope.
