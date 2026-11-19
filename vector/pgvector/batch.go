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

// GenerationBusyError means a lifecycle operation is changing the generation.
// Roll back the transaction and retry in a new transaction.
type GenerationBusyError struct{ Generation string }

func (e *GenerationBusyError) Error() string {
	return fmt.Sprintf("generation %s is busy", e.Generation)
}

// SaveDocuments saves documents in a READ COMMITTED transaction. It returns a
// key for each stale or missing input entry, in input order, including duplicates.
// Save documents in a consistent order, and roll back on any error. Retry a
// GenerationBusyError in a new transaction.
func (s *Store[K, G]) SaveDocuments(ctx context.Context, tx *sql.Tx, gen G, docs []Document[K]) ([]K, error) {
	var isolation string
	if err := tx.QueryRowContext(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
		return nil, err
	}
	if isolation != "read committed" {
		return nil, fmt.Errorf("SaveDocuments requires READ COMMITTED isolation, got %s", isolation)
	}
	row, err := s.lookup(ctx, tx, gen, false)
	if err != nil {
		return nil, err
	}
	var locked bool
	err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock_shared(hashtextextended($1::regclass::oid::text || ':' || $2::bigint::text, 0))`, s.generations(), row.id).Scan(&locked)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, &GenerationBusyError{Generation: string(gen)}
	}
	row, err = s.lookup(ctx, tx, gen, false)
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
