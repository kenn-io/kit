package vector

import (
	"context"
	"errors"
	"fmt"
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

		attempted := false
		for _, p := range pending {
			if _, ok := stale[p.Doc]; ok {
				continue
			}
			attempted = true

			chunks := Split(p.Content, o.Split)
			vectors, err := EncodeBatched(ctx, enc, chunks, o.Batch)
			skipped := false
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return stats, ctxErr
				}
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return stats, fmt.Errorf("encode document %v: %w", p.Doc, err)
				}
				if o.OnEncodeError == nil || !o.OnEncodeError(p.Doc, err) {
					return stats, fmt.Errorf("encode document %v: %w", p.Doc, err)
				}
				skipped = true
			}

			var cvs []ChunkVector
			if !skipped {
				cvs = make([]ChunkVector, len(chunks))
				for i, c := range chunks {
					cvs[i] = ChunkVector{ChunkIndex: c.Index, Vector: vectors[i]}
				}
			}
			if err := store.SaveVectors(ctx, gen, p.Doc, p.Revision, cvs); err != nil {
				if errors.Is(err, ErrStale) {
					stale[p.Doc] = struct{}{}
					stats.Stale++
					continue
				}
				return stats, fmt.Errorf("save document %v: %w", p.Doc, err)
			}
			if skipped {
				stats.Skipped++
				continue
			}
			stats.Documents++
			stats.Chunks += len(cvs)
		}
		if !attempted {
			// Nothing left, or only stale documents remain pending; either
			// way this run is done.
			return stats, nil
		}
	}
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
