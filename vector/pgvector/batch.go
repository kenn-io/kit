package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kit/search/postgres"
	"go.kenn.io/kit/vector"
)

// Document holds one document's revision and chunks to save.
type Document[K comparable] struct {
	Doc      K
	Revision any
	Vectors  []vector.ChunkVector
}

// SaveDocuments saves current documents in tx and returns stale or missing keys
// in input order. A repeated key replaces its earlier entry. The transaction
// must use READ COMMITTED. Document and lifecycle locks last until it ends;
// the caller must roll back on errors other than ErrRetired.
func (s *Store[K, G]) SaveDocuments(ctx context.Context, tx *sql.Tx, gen G, docs []Document[K]) ([]K, error) {
	if err := s.lockLifecycle(ctx, tx, true); err != nil {
		return nil, err
	}
	var isolation string
	if err := tx.QueryRowContext(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
		return nil, err
	}
	if isolation != "read committed" {
		return nil, fmt.Errorf("pgvector: SaveDocuments requires READ COMMITTED isolation, got %s", isolation)
	}
	row, err := s.lookup(ctx, tx, gen, false)
	if err != nil {
		return nil, err
	}
	if row.state == vector.StateRetired {
		return nil, vector.ErrRetired
	}
	if len(docs) == 0 {
		return nil, nil
	}
	keys := make([]K, len(docs))
	for i, doc := range docs {
		keys[i] = doc.Doc
	}
	key := postgres.QuoteIdentifier(s.schema.IDColumn)
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT 1 FROM %s WHERE %s=ANY($1) ORDER BY %s FOR NO KEY UPDATE`, postgres.QuoteIdentifier(s.schema.DocsTable), key, key), keys)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	var stale []K
	for _, doc := range docs {
		err = s.saveDocument(ctx, tx, row, doc.Doc, doc.Revision, doc.Vectors)
		if errors.Is(err, vector.ErrStale) {
			stale = append(stale, doc.Doc)
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	return stale, nil
}
