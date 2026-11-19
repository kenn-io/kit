package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kit/vector"
)

// Document holds one document's revision and chunks to save.
type Document[K comparable] struct {
	Doc      K
	Revision any
	Vectors  []vector.ChunkVector
}

// SaveDocuments saves documents in tx and returns stale or missing keys in input order.
// Callers should save documents in a consistent order and roll back on error.
func (s *Store[K, G]) SaveDocuments(ctx context.Context, tx *sql.Tx, gen G, docs []Document[K]) ([]K, error) {
	row, err := s.lookup(ctx, tx, gen, false)
	if err != nil {
		return nil, err
	}
	if row.state == vector.StateRetired {
		return nil, vector.ErrRetired
	}
	var stale []K
	for _, doc := range docs {
		err := s.saveDocument(ctx, tx, row, doc.Doc, doc.Revision, doc.Vectors)
		if errors.Is(err, vector.ErrStale) {
			stale = append(stale, doc.Doc)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("save document %v: %w", doc.Doc, err)
		}
	}
	return stale, nil
}
