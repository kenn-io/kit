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
	// Batch controls how chunks are batched into encode calls.
	Batch BatchOptions
	// Concurrency is the number of documents split and encoded in parallel
	// within each scan page. Values <= 0 use 1 (sequential). SaveVectors
	// and OnEncodeError stay serialized on the calling goroutine regardless,
	// so stores and hooks need no extra locking. It composes with
	// Batch.Concurrency, which parallelizes encode calls within a single
	// document's chunks.
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

// fillPage embeds one scan page of pending documents: workers split and
// encode up to o.Concurrency documents in parallel while the calling
// goroutine saves each result as it completes. The first save-side failure
// cancels the in-flight encodes, drains the workers, and is returned;
// results still in flight at that point are discarded and their documents
// stay pending for the next run.
func fillPage[K, G comparable](
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
