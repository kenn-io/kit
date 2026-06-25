package vector

import "context"

// Pending is one document that still needs embedding for a generation,
// paired with the text to embed.
type Pending[K comparable] struct {
	Doc     K
	Content string
}

// ChunkVector is a single chunk's embedding, ready to persist.
type ChunkVector struct {
	ChunkIndex int
	Vector     Vector
}

// Store is the persistence contract the Fill and Search flows depend on.
// Implementations are a function of the caller's source system — a SQLite,
// pgvector, or DuckDB table — and own all backend SQL and query
// construction. The flows never open a database or build SQL themselves.
//
// K is the caller's document key type and G its generation id type; the
// package compares both for equality but never interprets them.
type Store[K, G comparable] interface {
	// PendingForGeneration returns up to limit documents that are not yet
	// embedded for gen, in a stable order. Implementations typically scan
	// for "embed_gen IS NULL OR embed_gen <> gen". A document must stop
	// being reported once SaveVectors has persisted it for gen, so that a
	// fill loop terminates.
	PendingForGeneration(ctx context.Context, gen G, limit int) ([]Pending[K], error)

	// SaveVectors persists every chunk vector for doc under gen and marks
	// doc as embedded for gen (the scan-and-fill stamp).
	SaveVectors(ctx context.Context, gen G, doc K, vectors []ChunkVector) error

	// LiveGenerations returns the generations a search should query, in
	// descending preference. During a migration the building generation
	// precedes the active one, so Merge keeps the newer generation's hit
	// when a document appears in both.
	LiveGenerations(ctx context.Context) ([]G, error)

	// QueryGeneration returns chunk-level hits for query within gen,
	// ranked best first and capped at limit. This is where each backend's
	// vector query construction lives.
	QueryGeneration(ctx context.Context, gen G, query Vector, limit int) ([]Hit[K], error)
}
