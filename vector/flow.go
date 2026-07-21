package vector

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// FillOptions configures Fill. K is the document key type of the Store the
// fill runs over.
type FillOptions[K comparable] struct {
	// ScanBatch is the number of pending documents fetched per scan.
	// Values <= 0 use 128.
	ScanBatch int
	// Split controls how each document's content is windowed into chunks.
	Split SplitOptions
	// Batch controls how chunks are batched into encode calls. A positive
	// BatchSize packs chunks across documents within each scan page. Values
	// <= 0 preserve the per-document encode unit.
	Batch BatchOptions
	// Concurrency is the number of encode windows processed in parallel
	// within each scan page. Values <= 0 use 1 (sequential).
	// SaveVectors and OnEncodeError stay serialized on the calling goroutine
	// regardless, so stores and hooks need no extra locking. It composes with
	// Batch.Concurrency, which parallelizes encode calls within each window.
	Concurrency int
	// OnEncodeError, if non-nil, is consulted when encoding a document
	// fails. Returning true skips the document: it is stamped for the
	// generation with no vectors so it stops being pending, and the fill
	// continues (the treatment for inputs a model permanently rejects).
	// Returning false — or leaving OnEncodeError nil — aborts the fill
	// with the error, which is the right default for transient failures.
	OnEncodeError func(doc K, err error) bool
}

// FillStats reports what a Fill run embedded.
type FillStats struct {
	// Documents is the number of documents embedded and stamped.
	Documents int
	// Chunks is the total chunk vectors saved across Documents.
	Chunks int
	// Skipped counts documents stamped without vectors because
	// OnEncodeError elected to skip them.
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
// When Batch.BatchSize is positive, chunks from adjacent documents in one
// scan page may share an encode call. A failed shared call is retried at
// document granularity before OnEncodeError is consulted. If those retries
// cannot attribute the failure to a document, Fill aborts rather than silently
// converting a request-shape or transport failure into successful documents.
func Fill[K, G comparable](ctx context.Context, store Store[K, G], gen G, enc EncodeFunc, o FillOptions[K]) (FillStats, error) {
	scanBatch := o.ScanBatch
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
	refs      []fillChunkRef
	vectors   []Vector
	docErrors map[int]error
	fatal     error
}

type fillWindowResult struct {
	batches []fillBatchResult
}

// fillPage embeds one scan page. A positive BatchSize packs fixed-size chunk
// batches across documents, then scatters vectors back before saving each
// document independently. The legacy per-document path remains in use when
// BatchSize is unset, preserving its unbounded-per-document call shape.
func fillPage[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc,
	o FillOptions[K], docs []Pending[K], stale map[K]struct{}, stats *FillStats,
) error {
	if o.Batch.BatchSize <= 0 {
		return fillPageByDocument(ctx, store, gen, enc, o, docs, stale, stats)
	}
	return fillPageAcrossDocuments(ctx, store, gen, enc, o, docs, stale, stats)
}

func fillPageAcrossDocuments[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc,
	o FillOptions[K], docs []Pending[K], stale map[K]struct{}, stats *FillStats,
) error {
	documentStates := make([]fillDocumentState[K], len(docs))
	var refs []fillChunkRef
	for doc, pending := range docs {
		chunks := Split(pending.Content, o.Split)
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

	nextSave := 0
	if err := saveReadyDocuments(ctx, store, gen, o, documentStates, &nextSave, stale, stats); err != nil {
		return err
	}
	if len(refs) == 0 {
		return nil
	}

	batchConcurrency := o.Batch.Concurrency
	if batchConcurrency <= 0 {
		batchConcurrency = 1
	}
	windowSize := len(refs)
	if o.Batch.BatchSize <= len(refs)/batchConcurrency {
		windowSize = o.Batch.BatchSize * batchConcurrency
	}
	windowCount := 1 + (len(refs)-1)/windowSize
	workers := min(max(o.Concurrency, 1), windowCount)
	if workers <= 1 {
		for start := 0; start < len(refs); start += windowSize {
			end := min(start+windowSize, len(refs))
			active := activeFillRefs(refs[start:end], documentStates)
			if len(active) == 0 {
				continue
			}
			result := encodeFillWindow(ctx, enc, active, o.Batch.BatchSize)
			if err := applyFillWindow(ctx, store, gen, o, result, documentStates,
				&nextSave, stale, stats); err != nil {
				return err
			}
		}
		if nextSave != len(documentStates) {
			return fmt.Errorf("fill page: %d documents were not completed", len(documentStates)-nextSave)
		}
		return nil
	}

	encCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan []fillChunkRef)
	results := make(chan fillWindowResult)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for window := range jobs {
				result := encodeFillWindow(encCtx, enc, window, o.Batch.BatchSize)
				select {
				case results <- result:
				case <-encCtx.Done():
					return
				}
			}
		})
	}
	go func() {
		defer close(jobs)
		for start := 0; start < len(refs); start += windowSize {
			end := min(start+windowSize, len(refs))
			select {
			case jobs <- refs[start:end]:
			case <-encCtx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	var pageErr error
	for result := range results {
		if pageErr != nil {
			continue
		}
		if err := applyFillWindow(ctx, store, gen, o, result, documentStates,
			&nextSave, stale, stats); err != nil {
			pageErr = err
			cancel()
		}
	}
	if pageErr != nil {
		return pageErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if nextSave != len(documentStates) {
		return fmt.Errorf("fill page: %d documents were not completed", len(documentStates)-nextSave)
	}
	return nil
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

func encodeFillWindow(ctx context.Context, enc EncodeFunc, refs []fillChunkRef, batchSize int) fillWindowResult {
	batchCount := 1 + (len(refs)-1)/batchSize
	result := fillWindowResult{batches: make([]fillBatchResult, batchCount)}
	if batchCount == 1 {
		result.batches[0] = encodeFillBatch(ctx, enc, refs)
		return result
	}

	var wg sync.WaitGroup
	for batch := range batchCount {
		start := batch * batchSize
		end := min(start+batchSize, len(refs))
		wg.Go(func() {
			result.batches[batch] = encodeFillBatch(ctx, enc, refs[start:end])
		})
	}
	wg.Wait()
	return result
}

func encodeFillBatch(ctx context.Context, enc EncodeFunc, refs []fillChunkRef) fillBatchResult {
	result := fillBatchResult{
		refs:      refs,
		vectors:   make([]Vector, len(refs)),
		docErrors: make(map[int]error),
	}
	chunks := make([]Chunk, len(refs))
	for i, ref := range refs {
		chunks[i] = ref.value
	}

	vectors, err := EncodeBatched(ctx, enc, chunks, BatchOptions{})
	if err == nil {
		result.vectors = vectors
		return result
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		result.fatal = err
		return result
	}

	// A one-document batch already has exact error attribution. Retrying it
	// would only duplicate the same request; translate typed chunk indexes
	// back to their position within the document and return the failure.
	if refs[0].doc == refs[len(refs)-1].doc {
		result.docErrors[refs[0].doc] = offsetInvalidVectorError(err, refs[0].chunk)
		return result
	}

	// An encoder error for a cross-document batch cannot identify which
	// document was rejected. Retry each document's contiguous slice only on
	// this error path, so OnEncodeError is still consulted per document and
	// unaffected neighbors can be saved.
	for start := 0; start < len(refs); {
		end := start + 1
		for end < len(refs) && refs[end].doc == refs[start].doc {
			end++
		}
		segment := chunks[start:end]
		segmentVectors, segmentErr := EncodeBatched(ctx, enc, segment, BatchOptions{})
		if segmentErr != nil {
			if ctx.Err() != nil || errors.Is(segmentErr, context.Canceled) || errors.Is(segmentErr, context.DeadlineExceeded) {
				result.fatal = segmentErr
				return result
			}
			result.docErrors[refs[start].doc] = offsetInvalidVectorError(segmentErr, refs[start].chunk)
		} else {
			copy(result.vectors[start:end], segmentVectors)
		}
		start = end
	}
	if len(result.docErrors) == 0 {
		result.fatal = fmt.Errorf("cross-document batch failed but no document failed in isolation: %w", err)
	}
	return result
}

func offsetInvalidVectorError(err error, chunkOffset int) error {
	var invalid *InvalidVectorError
	if !errors.As(err, &invalid) {
		return err
	}
	translated := *invalid
	translated.Chunk += chunkOffset
	return fmt.Errorf("encode document chunks at %d: %w", chunkOffset, &translated)
}

func applyFillWindow[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, o FillOptions[K], result fillWindowResult,
	states []fillDocumentState[K], nextSave *int, stale map[K]struct{}, stats *FillStats,
) error {
	for _, batch := range result.batches {
		if batch.fatal != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			for _, ref := range batch.refs {
				if states[ref.doc].saved {
					continue
				}
				return fmt.Errorf("encode batch beginning with document %v: %w",
					states[ref.doc].encoded.doc.Doc, batch.fatal)
			}
			return batch.fatal
		}

		for doc, err := range batch.docErrors {
			state := &states[doc]
			if state.saved || state.failed {
				continue
			}
			state.encoded.err = err
			state.remaining = 0
			state.failed = true
		}
		for i, ref := range batch.refs {
			state := &states[ref.doc]
			if state.saved || state.failed {
				continue
			}
			state.encoded.vectors[ref.chunk] = batch.vectors[i]
			state.remaining--
		}
		if err := saveReadyDocuments(ctx, store, gen, o, states, nextSave, stale, stats); err != nil {
			return err
		}
	}
	return nil
}

func saveReadyDocuments[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, o FillOptions[K],
	states []fillDocumentState[K], nextSave *int, stale map[K]struct{}, stats *FillStats,
) error {
	for *nextSave < len(states) && states[*nextSave].remaining == 0 {
		state := &states[*nextSave]
		if err := saveEncoded(ctx, store, gen, o, state.encoded, stale, stats); err != nil {
			return err
		}
		state.saved = true
		*nextSave = *nextSave + 1
	}
	return nil
}

// fillPageByDocument is the original per-document path used when BatchSize is
// unset. Workers split and encode up to o.Concurrency documents in parallel
// while the calling goroutine saves each result as it completes. The first
// save-side failure cancels in-flight encodes and is returned.
func fillPageByDocument[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, enc EncodeFunc,
	o FillOptions[K], docs []Pending[K], stale map[K]struct{}, stats *FillStats,
) error {
	workers := min(o.Concurrency, len(docs))
	if workers <= 1 {
		// Concurrency <= 1 promises strictly sequential behavior: encode,
		// then save, then take up the next document. The worker pipeline
		// below would keep one encode in flight while the caller saves — an
		// extra encoder/API call the options said would not be made, issued
		// even as a failing save is about to abort the fill.
		for _, p := range docs {
			chunks := Split(p.Content, o.Split)
			vectors, err := EncodeBatched(ctx, enc, chunks, o.Batch)
			r := fillEncoded[K]{doc: p, chunks: chunks, vectors: vectors, err: err}
			if err := saveEncoded(ctx, store, gen, o, r, stale, stats); err != nil {
				return err
			}
		}
		return nil
	}

	encCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan Pending[K])
	results := make(chan fillEncoded[K])
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for p := range jobs {
				chunks := Split(p.Content, o.Split)
				vectors, err := EncodeBatched(encCtx, enc, chunks, o.Batch)
				select {
				case results <- fillEncoded[K]{doc: p, chunks: chunks, vectors: vectors, err: err}:
				case <-encCtx.Done():
					return
				}
			}
		})
	}
	go func() {
		defer close(jobs)
		for _, p := range docs {
			select {
			case jobs <- p:
			case <-encCtx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	var pageErr error
	for r := range results {
		if pageErr != nil {
			continue // draining after cancel
		}
		if err := saveEncoded(ctx, store, gen, o, r, stale, stats); err != nil {
			pageErr = err
			cancel()
		}
	}
	return pageErr
}

// saveEncoded applies one document's encode outcome: it consults
// OnEncodeError for failures, stamps skips, saves vectors, and records
// stale revisions, updating stats to match. It runs on Fill's calling
// goroutine only.
func saveEncoded[K, G comparable](
	ctx context.Context, store Store[K, G], gen G, o FillOptions[K],
	r fillEncoded[K], stale map[K]struct{}, stats *FillStats,
) error {
	skipped := false
	if r.err != nil {
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
		vectors, err := EncodeBatched(ctx, enc, []Chunk{{Index: 0, Text: queryText}}, BatchOptions{})
		if err != nil {
			return nil, fmt.Errorf("embed query for generation %v: %w", gen, err)
		}
		hits, err := store.QueryGeneration(ctx, gen, vectors[0], perGen)
		if err != nil {
			return nil, fmt.Errorf("query generation %v: %w", gen, err)
		}
		lists = append(lists, RollupByDocument(hits))
	}
	return Merge(lists, o.Merge), nil
}
