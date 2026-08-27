package vector

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type fillOptions[K comparable] struct {
	scanBatch               int
	split                   SplitOptions
	batch                   batchOptions
	concurrency             int
	onEncodeError           func(doc K, err error) bool
	shouldIsolateBatchError func(error) bool
}

// FillOption configures a Fill call.
type FillOption[K comparable] func(*fillOptions[K])

// WithFillScanBatch sets the number of pending documents fetched per scan.
// Values less than or equal to zero use 128.
func WithFillScanBatch[K comparable](size int) FillOption[K] {
	return func(o *fillOptions[K]) { o.scanBatch = size }
}

// WithFillSplit controls how each document's content is windowed into chunks.
func WithFillSplit[K comparable](options SplitOptions) FillOption[K] {
	return func(o *fillOptions[K]) { o.split = options }
}

// WithFillBatch controls how chunks are grouped into encode calls. A positive
// WithBatchSize packs chunks across documents within each scan page. Omitting
// it preserves the per-document encode unit.
func WithFillBatch[K comparable](options ...BatchOption) FillOption[K] {
	return func(o *fillOptions[K]) { o.batch = applyBatchOptions(options) }
}

// WithFillConcurrency bounds parallel fill work within each scan page. Values
// less than or equal to zero use one worker. SaveVectors and the error hooks
// stay serialized on the calling goroutine. When WithBatchSize is positive,
// this limit composes with WithBatchConcurrency; their product bounds the
// concurrent encode calls.
func WithFillConcurrency[K comparable](concurrency int) FillOption[K] {
	return func(o *fillOptions[K]) { o.concurrency = concurrency }
}

// WithFillEncodeError handles a document that fails to encode. Returning true
// skips and stamp-saves that document so later Fill calls do not retry it.
// Returning false aborts Fill, which is the right default for transient
// failures. Omitting this option also aborts on the first encode error.
func WithFillEncodeError[K comparable](handler func(doc K, err error) bool) FillOption[K] {
	return func(o *fillOptions[K]) { o.onEncodeError = handler }
}

// WithFillBatchErrorIsolation decides whether a failed shared encode call is
// worth diagnosing at document-slice granularity. Returning true permits
// diagnosis only; WithFillEncodeError still decides whether to skip an
// attributed document. Fill bypasses this handler for single-document calls,
// context errors, and errors with an exact *InvalidVectorError position.
func WithFillBatchErrorIsolation[K comparable](classify func(error) bool) FillOption[K] {
	return func(o *fillOptions[K]) { o.shouldIsolateBatchError = classify }
}

func applyFillOptions[K comparable](options []FillOption[K]) fillOptions[K] {
	o := fillOptions[K]{}
	for _, option := range options {
		if option != nil {
			option(&o)
		}
	}
	return o
}

// FillStats reports what a Fill run embedded.
type FillStats struct {
	// Documents is the number of documents embedded and stamped.
	Documents int
	// Chunks is the total chunk vectors saved across Documents.
	Chunks int
	// Skipped counts documents stamped without vectors because
	// the WithFillEncodeError handler elected to skip them.
	Skipped int
	// Stale counts documents left pending because they changed between
	// scan and save (SaveVectors returned ErrStale). A later run retries
	// them at their new revision.
	Stale int
}

// Fill embeds every document that still needs the target generation: it
// scans the store for pending documents, splits and encodes each, and
// saves the resulting vectors, repeating until no documents remain. It is
// the generic scan-and-fill loop; the store decides what counts as
// pending and persists the results.
//
// A document that changes mid-run (ErrStale from SaveVectors) is left
// pending and not retried until the next Fill call, so an actively edited
// document cannot starve the loop.
//
// When WithFillBatch includes a positive WithBatchSize, chunks from adjacent
// documents in one scan page may share an encode call. Errors with exact
// document attribution go directly to the WithFillEncodeError handler. Other
// shared-call errors are diagnosed at document-slice granularity only when
// WithFillBatchErrorIsolation permits it. Omitting that option aborts without
// document-level retries. WithFillEncodeError remains the sole authority for
// skip-stamping an attributed document.
func Fill[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc,
	options ...FillOption[K],
) (FillStats, error) {
	o := applyFillOptions(options)
	if err := ctx.Err(); err != nil {
		return FillStats{}, err
	}
	if err := o.batch.validate(); err != nil {
		return FillStats{}, err
	}
	scanBatch := o.scanBatch
	if scanBatch <= 0 {
		scanBatch = 128
	}

	var stats FillStats
	stale := make(map[K]struct{})
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		// Stale documents remain pending and occupy scan slots; widening
		// the limit keeps fresh documents visible past them.
		pending, err := store.PendingForGeneration(ctx, gen, scanBatch+len(stale))
		if err != nil {
			return stats, fmt.Errorf("scan pending: %w", err)
		}

		docs := make([]Pending[K], 0, len(pending))
		for _, p := range pending {
			if _, ok := stale[p.Doc]; !ok {
				docs = append(docs, p)
			}
		}
		if len(docs) == 0 {
			// Nothing left, or only stale documents remain pending; either
			// way this run is done.
			return stats, nil
		}
		if err := fillPage(ctx, store, gen, enc, o, docs, stale, &stats); err != nil {
			return stats, err
		}
	}
}

// fillEncoded carries one document's encode outcome from a worker to the
// collecting goroutine.
type fillEncoded[K comparable] struct {
	doc     Pending[K]
	chunks  []Chunk
	vectors []Vector
	err     error
	skip    bool
}

type fillDocumentState[K comparable] struct {
	encoded   fillEncoded[K]
	remaining int
	failed    bool
	saved     bool
}

type fillChunkRef struct {
	doc   int
	chunk int
	value Chunk
}

type fillBatchResult struct {
	refs    []fillChunkRef
	vectors []Vector
	err     error
}

// fillPage embeds one scan page. A positive batch size packs fixed-size chunk
// batches across documents, then scatters vectors back before saving each
// document independently. The legacy per-document path remains in use when
// the batch size is unset, preserving its unbounded-per-document call shape.
func fillPage[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc,
	o fillOptions[K], docs []Pending[K], stale map[K]struct{}, stats *FillStats,
) error {
	if o.batch.batchSize <= 0 || enc == nil {
		return fillPageByDocument(ctx, store, gen, enc, o, docs, stale, stats)
	}
	return fillPageAcrossDocuments(ctx, store, gen, enc, o, docs, stale, stats)
}

func fillPageAcrossDocuments[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc,
	o fillOptions[K], docs []Pending[K], stale map[K]struct{}, stats *FillStats,
) error {
	documentStates := make([]fillDocumentState[K], len(docs))
	var refs []fillChunkRef
	for doc, pending := range docs {
		chunks := Split(pending.Content, o.split)
		documentStates[doc] = fillDocumentState[K]{
			encoded: fillEncoded[K]{
				doc:     pending,
				chunks:  chunks,
				vectors: make([]Vector, len(chunks)),
			},
			remaining: len(chunks),
		}
		for chunk, value := range chunks {
			refs = append(refs, fillChunkRef{doc: doc, chunk: chunk, value: value})
		}
	}

	if len(refs) == 0 {
		return saveReadyDocuments(ctx, store, gen, o, documentStates, true, stale, stats)
	}

	batchSize := o.batch.effectiveBatchSize(len(refs))
	orderedSaves := o.concurrency <= 1
	if err := saveReadyDocuments(ctx, store, gen, o, documentStates, orderedSaves, stale, stats); err != nil {
		return err
	}

	batchConcurrency := max(o.batch.concurrency, 1)
	batches := splitFillRefs(refs, batchSize)
	encode := func(workCtx context.Context, batch []fillChunkRef) fillBatchResult {
		return encodeFillBatch(workCtx, enc, batch)
	}
	var err error
	if orderedSaves {
		// A sequential fill completes and saves one bounded window before
		// starting another, so a save failure cannot launch later work.
		for start := 0; start < len(batches); start += batchConcurrency {
			window := batches[start:min(start+batchConcurrency, len(batches))]
			active := make([][]fillChunkRef, 0, len(window))
			for _, batch := range window {
				batch = activeFillRefs(batch, documentStates)
				if len(batch) > 0 {
					active = append(active, batch)
				}
			}
			err = runFillJobs(ctx, batchConcurrency, active, encode,
				func(result fillBatchResult) bool { return result.err != nil },
				func(batch fillBatchResult) error {
					return applyFillBatch(ctx, store, gen, o, enc, batch, documentStates,
						true, stale, stats)
				})
			if err != nil {
				break
			}
		}
	} else {
		// Schedule individual batches so a completed document can be saved
		// without waiting for an unrelated slow batch in the same window.
		// Already-dispatched work may finish after another batch skips one of
		// its documents; applyFillBatch discards those results without racing
		// workers against the serialized document state.
		workers := len(batches)
		// Compute the worker and batch concurrency product without
		// overflowing the product.
		if o.concurrency <= len(batches)/batchConcurrency {
			workers = o.concurrency * batchConcurrency
		}
		err = runFillJobs(ctx, workers, batches, encode,
			func(result fillBatchResult) bool { return result.err != nil },
			func(batch fillBatchResult) error {
				return applyFillBatch(ctx, store, gen, o, enc, batch, documentStates,
					false, stale, stats)
			})
	}
	if err != nil {
		return err
	}
	if incomplete := incompleteFillDocuments(documentStates); incomplete > 0 {
		return fmt.Errorf("fill page: %d documents were not completed", incomplete)
	}
	return nil
}

func splitFillRefs(refs []fillChunkRef, size int) [][]fillChunkRef {
	parts := make([][]fillChunkRef, 0, 1+(len(refs)-1)/size)
	for start := 0; start < len(refs); start += size {
		parts = append(parts, refs[start:min(start+size, len(refs))])
	}
	return parts
}

func activeFillRefs[K comparable](refs []fillChunkRef, states []fillDocumentState[K]) []fillChunkRef {
	active := make([]fillChunkRef, 0, len(refs))
	for _, ref := range refs {
		if !states[ref.doc].failed && !states[ref.doc].saved {
			active = append(active, ref)
		}
	}
	return active
}

func encodeFillBatch(ctx context.Context, enc EncodeFunc, refs []fillChunkRef) fillBatchResult {
	chunks := make([]Chunk, len(refs))
	for i, ref := range refs {
		chunks[i] = ref.value
	}
	vectors, err := EncodeBatched(ctx, enc, chunks)
	return fillBatchResult{refs: refs, vectors: vectors, err: err}
}

func offsetInvalidVectorError(err error, chunkOffset int) error {
	var invalid *InvalidVectorError
	if !errors.As(err, &invalid) {
		return err
	}
	translated := *invalid
	translated.Chunk += chunkOffset
	return fmt.Errorf("encode document chunks at %d: %w", chunkOffset, errors.Join(&translated, err))
}

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
	ctx context.Context, o fillOptions[K], states []fillDocumentState[K], doc int, err error,
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
	if o.onEncodeError == nil || !o.onEncodeError(state.encoded.doc.Doc, err) {
		return fmt.Errorf("encode document %v: %w", state.encoded.doc.Doc, err)
	}
	state.encoded.err = err
	state.encoded.skip = true
	state.remaining = 0
	state.failed = true
	return nil
}

func probeFillDocumentSlices[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc, o fillOptions[K],
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

func applyAttributedInvalidFillBatch[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc, o fillOptions[K],
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

func applyFillBatch[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, o fillOptions[K], enc EncodeFunc,
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
	if invalid, ok := errors.AsType[*InvalidVectorError](batch.err); ok {
		return applyAttributedInvalidFillBatch(ctx, store, gen, enc, o, batch, invalid,
			states, orderedSaves, stale, stats)
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
	if o.shouldIsolateBatchError == nil || !o.shouldIsolateBatchError(batch.err) {
		return fillBatchContextError(batch.err, active, states)
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

func saveReadyDocuments[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, o fillOptions[K],
	states []fillDocumentState[K], ordered bool, stale map[K]struct{}, stats *FillStats,
) error {
	for i := range states {
		state := &states[i]
		if state.saved {
			continue
		}
		if state.remaining != 0 {
			if ordered {
				break
			}
			continue
		}
		if err := saveEncoded(ctx, store, gen, o, state.encoded, stale, stats); err != nil {
			return err
		}
		state.saved = true
	}
	return nil
}

func incompleteFillDocuments[K comparable](states []fillDocumentState[K]) int {
	incomplete := 0
	for _, state := range states {
		if !state.saved {
			incomplete++
		}
	}
	return incomplete
}

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

// fillPageByDocument is the original per-document path used when the batch
// size is unset. Workers split and encode up to o.concurrency documents in parallel
// while the calling goroutine saves each result as it completes. The first
// save-side failure cancels in-flight encodes and is returned.
func fillPageByDocument[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc,
	o fillOptions[K], docs []Pending[K], stale map[K]struct{}, stats *FillStats,
) error {
	return runFillJobs(ctx, o.concurrency, docs,
		func(workCtx context.Context, p Pending[K]) fillEncoded[K] {
			chunks := Split(p.Content, o.split)
			vectors, err := encodeBatched(workCtx, enc, chunks, o.batch)
			return fillEncoded[K]{doc: p, chunks: chunks, vectors: vectors, err: err}
		},
		nil,
		func(result fillEncoded[K]) error {
			return saveEncoded(ctx, store, gen, o, result, stale, stats)
		})
}

// saveEncoded applies one document's encode outcome: it consults the encode
// error handler for failures, stamps skips, saves vectors, and records
// stale revisions, updating stats to match. It runs on Fill's calling
// goroutine only.
func saveEncoded[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, o fillOptions[K],
	r fillEncoded[K], stale map[K]struct{}, stats *FillStats,
) error {
	skipped := r.skip
	if r.err != nil && !skipped {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(r.err, context.Canceled) || errors.Is(r.err, context.DeadlineExceeded) {
			return fmt.Errorf("encode document %v: %w", r.doc.Doc, r.err)
		}
		if o.onEncodeError == nil || !o.onEncodeError(r.doc.Doc, r.err) {
			return fmt.Errorf("encode document %v: %w", r.doc.Doc, r.err)
		}
		skipped = true
	}

	var cvs []ChunkVector
	if !skipped {
		cvs = make([]ChunkVector, len(r.chunks))
		for i, c := range r.chunks {
			cvs[i] = ChunkVector{ChunkIndex: c.Index, Vector: r.vectors[i]}
		}
	}
	if err := store.SaveVectors(ctx, gen, r.doc.Doc, r.doc.Revision, cvs); err != nil {
		if errors.Is(err, ErrStale) {
			stale[r.doc.Doc] = struct{}{}
			stats.Stale++
			return nil
		}
		return fmt.Errorf("save document %v: %w", r.doc.Doc, err)
	}
	if skipped {
		stats.Skipped++
		return nil
	}
	stats.Documents++
	stats.Chunks += len(cvs)
	return nil
}

// SearchOptions configures Search.
type SearchOptions struct {
	// PerGeneration caps how many hits are fetched from each generation
	// before merging. Values <= 0 use 50.
	PerGeneration int
	// Merge configures how per-generation results are combined.
	Merge MergeOptions
}

// Search embeds queryText once per live generation (each may use a
// different model), queries each generation, rolls the chunk hits up to
// documents, and merges the per-generation results into one ranking.
// encFor maps a generation to the encoder for that generation's model.
func Search[K, G comparable](
	ctx context.Context,
	store Store[K, G],
	queryText string,
	encFor func(gen G) EncodeFunc,
	o SearchOptions,
) ([]Hit[K], error) {
	perGen := o.PerGeneration
	if perGen <= 0 {
		perGen = 50
	}

	gens, err := store.LiveGenerations(ctx)
	if err != nil {
		return nil, fmt.Errorf("live generations: %w", err)
	}

	lists := make([][]Hit[K], 0, len(gens))
	for _, gen := range gens {
		enc := encFor(gen)
		if enc == nil {
			return nil, fmt.Errorf("no encoder for generation %v", gen)
		}
		vectors, err := EncodeBatched(ctx, enc, []Chunk{{Index: 0, Text: queryText}})
		if err != nil {
			return nil, fmt.Errorf("embed query for generation %v: %w", gen, err)
		}
		if len(vectors) != 1 {
			return nil, fmt.Errorf("embed query for generation %v returned %d vectors, want 1", gen, len(vectors))
		}
		hits, err := store.QueryGeneration(ctx, gen, vectors[0], perGen)
		if err != nil {
			return nil, fmt.Errorf("query generation %v: %w", gen, err)
		}
		lists = append(lists, RollupByDocument(hits))
	}
	return Merge(lists, o.Merge), nil
}
