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
	if err := s.lockGeneration(ctx, tx, gen, true); err != nil {
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
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT ordinal FROM (SELECT DISTINCT ON (doc_key) ordinal FROM unnest(ARRAY(SELECT %s FROM %s WHERE false) || $1) WITH ORDINALITY AS input(doc_key,ordinal) ORDER BY doc_key,ordinal DESC) last ORDER BY ordinal`, key, postgres.QuoteIdentifier(s.schema.DocsTable)), keys)
	if err != nil {
		return nil, err
	}
	var last []int
	for rows.Next() {
		var ordinal int
		if err := rows.Scan(&ordinal); err != nil {
			_ = rows.Close()
			return nil, err
		}
		last = append(last, ordinal-1)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SELECT 1 FROM %s WHERE %s=ANY($1) ORDER BY %s FOR NO KEY UPDATE`, postgres.QuoteIdentifier(s.schema.DocsTable), key, key), keys); err != nil {
		return nil, err
	}
	var stale []K
	for _, i := range last {
		doc := docs[i]
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
