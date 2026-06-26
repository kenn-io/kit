package vector

import (
	"context"
	"fmt"
)

// FillOptions configures Fill.
type FillOptions struct {
	// ScanBatch is the number of pending documents fetched per scan.
	// Values <= 0 use 128.
	ScanBatch int
	// Split controls how each document's content is windowed into chunks.
	Split SplitOptions
	// Batch controls how chunks are batched into encode calls.
	Batch BatchOptions
}

// FillStats reports what a Fill run embedded.
type FillStats struct {
	Documents int
	Chunks    int
}

// Fill embeds every document that still needs the target generation: it
// scans the store for pending documents, splits and encodes each, and
// saves the resulting vectors, repeating until no documents remain. It is
// the generic scan-and-fill loop; the store decides what counts as
// pending and persists the results.
func Fill[K, G comparable](ctx context.Context, store Store[K, G], gen G, enc EncodeFunc, o FillOptions) (FillStats, error) {
	scanBatch := o.ScanBatch
	if scanBatch <= 0 {
		scanBatch = 128
	}

	var stats FillStats
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		pending, err := store.PendingForGeneration(ctx, gen, scanBatch)
		if err != nil {
			return stats, fmt.Errorf("scan pending: %w", err)
		}
		if len(pending) == 0 {
			return stats, nil
		}

		for _, p := range pending {
			chunks := Split(p.Content, o.Split)
			vectors, err := EncodeBatched(ctx, enc, chunks, o.Batch)
			if err != nil {
				return stats, fmt.Errorf("encode document %v: %w", p.Doc, err)
			}
			cvs := make([]ChunkVector, len(chunks))
			for i, c := range chunks {
				cvs[i] = ChunkVector{ChunkIndex: c.Index, Vector: vectors[i]}
			}
			if err := store.SaveVectors(ctx, gen, p.Doc, cvs); err != nil {
				return stats, fmt.Errorf("save document %v: %w", p.Doc, err)
			}
			stats.Documents++
			stats.Chunks += len(cvs)
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
