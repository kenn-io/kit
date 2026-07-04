package vector

import "sort"

// Hit is a single search result identifying the document it belongs to. K
// is the caller's document key type (for example int64 or a UUID); this
// package compares keys for equality but never interprets them.
type Hit[K comparable] struct {
	// Doc identifies the source document.
	Doc K
	// ChunkIndex is the chunk within Doc that matched.
	ChunkIndex int
	// Score is the backend's similarity score for this chunk. Merge
	// overwrites it with the merged score under the chosen strategy.
	Score float32
}

// RollupByDocument reduces chunk-level hits to one hit per document,
// keeping the highest-scoring chunk for each, and returns them sorted by
// score descending. It is the chunk->document step a caller applies to a
// single generation's results before merging across generations.
func RollupByDocument[K comparable](hits []Hit[K]) []Hit[K] {
	if len(hits) == 0 {
		return nil
	}
	best := make(map[K]Hit[K], len(hits))
	order := make([]K, 0, len(hits))
	for _, h := range hits {
		cur, ok := best[h.Doc]
		if !ok {
			order = append(order, h.Doc)
			best[h.Doc] = h
			continue
		}
		if h.Score > cur.Score {
			best[h.Doc] = h
		}
	}
	out := make([]Hit[K], 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// MergeStrategy selects how Merge orders documents drawn from different
// generations, whose raw scores are not directly comparable.
type MergeStrategy int

const (
	// MergeNormalizedScore min-max normalizes each generation's scores to
	// [0,1] before ordering. It is the default: it keeps score signal
	// without letting one generation's score scale dominate.
	MergeNormalizedScore MergeStrategy = iota
	// MergeRawScore orders by raw score. Use it only when the generations
	// share a model family and comparable score distributions.
	MergeRawScore
	// MergeReciprocalRank ignores absolute scores and fuses by rank. Use
	// it when score distributions differ sharply between generations.
	MergeReciprocalRank
)

// MergeOptions configures Merge.
type MergeOptions struct {
	// Strategy selects the ordering policy. The zero value is
	// MergeNormalizedScore.
	Strategy MergeStrategy
	// RankConstant is the k term in reciprocal-rank fusion. Values <= 0
	// use 60.
	RankConstant float64
	// Limit caps the number of returned hits. Values <= 0 return all.
	Limit int
}

// Merge unions per-generation, document-level result lists into one
// ranking. The lists are given in descending preference: when a document
// appears in more than one list, the hit from the earliest list is kept,
// which is how a caller expresses "prefer the newer generation" during a
// migration. Coverage is a union, so a document present in only one
// generation is never dropped.
//
// Each surviving hit's Score is set to the merged score under the chosen
// strategy, and the result is ordered by that score descending.
func Merge[K comparable](perGeneration [][]Hit[K], o MergeOptions) []Hit[K] {
	rep := make(map[K]Hit[K])
	order := make([]K, 0)
	score := make(map[K]float64)

	switch o.Strategy {
	case MergeReciprocalRank:
		k := o.RankConstant
		if k <= 0 {
			k = 60
		}
		for _, list := range perGeneration {
			for rank, h := range list {
				if _, ok := rep[h.Doc]; !ok {
					rep[h.Doc] = h
					order = append(order, h.Doc)
				}
				score[h.Doc] += 1.0 / (k + float64(rank) + 1.0)
			}
		}
	case MergeRawScore:
		for _, list := range perGeneration {
			for _, h := range list {
				if _, ok := rep[h.Doc]; ok {
					continue
				}
				rep[h.Doc] = h
				order = append(order, h.Doc)
				score[h.Doc] = float64(h.Score)
			}
		}
	default: // MergeNormalizedScore
		for _, list := range perGeneration {
			lo, hi := scoreRange(list)
			span := hi - lo
			for _, h := range list {
				if _, ok := rep[h.Doc]; ok {
					continue
				}
				rep[h.Doc] = h
				order = append(order, h.Doc)
				if span > 0 {
					score[h.Doc] = float64(h.Score-lo) / float64(span)
				} else {
					score[h.Doc] = 1
				}
			}
		}
	}

	out := make([]Hit[K], 0, len(order))
	for _, doc := range order {
		h := rep[doc]
		h.Score = float32(score[doc])
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if o.Limit > 0 && len(out) > o.Limit {
		out = out[:o.Limit]
	}
	return out
}

func scoreRange[K comparable](hits []Hit[K]) (lo, hi float32) {
	for i, h := range hits {
		if i == 0 || h.Score < lo {
			lo = h.Score
		}
		if i == 0 || h.Score > hi {
			hi = h.Score
		}
	}
	return lo, hi
}
