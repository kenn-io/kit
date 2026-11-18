// Package pgvector implements vector.Store using PostgreSQL and pgvector.
// The caller owns the database connection and documents table. RevisionColumn
// must change whenever document content changes in place.
package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kit/search/postgres"
)

// Schema names the documents table and managed tables. All names are bare identifiers.
type Schema struct {
	DocsTable         string
	IDColumn          string
	ContentColumn     string
	RevisionColumn    string
	Prefix            string
	GenerationKeyType string
}

func (s *Schema) validate() error {
	if s.GenerationKeyType == "" {
		s.GenerationKeyType = "text"
	}
	for name, value := range map[string]string{"docs table": s.DocsTable, "id column": s.IDColumn, "content column": s.ContentColumn, "revision column": s.RevisionColumn, "prefix": s.Prefix, "generation key type": s.GenerationKeyType} {
		if !postgres.ValidIdentifier(value) || len(value) > 63 {
			return fmt.Errorf("invalid %s %q", name, value)
		}
	}
	// Leave room for a 19-digit generation ID and the HNSW index suffix.
	if len(s.Prefix) > 30 {
		return fmt.Errorf("prefix %q is too long", s.Prefix)
	}
	return nil
}
func quote(s string) string { return postgres.QuoteIdentifier(s) }

// ErrPgvectorMissing means the database has no pgvector halfvec type.
var ErrPgvectorMissing = errors.New("pgvector halfvec is unavailable")

func extensionSchema(ctx context.Context, db *sql.DB) (string, error) {
	var schema, version string
	err := db.QueryRowContext(ctx, `SELECT n.nspname, e.extversion FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='vector'`).Scan(&schema, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrPgvectorMissing
	}
	if err != nil {
		return "", fmt.Errorf("find pgvector: %w", err)
	}
	var major, minor int
	if _, err := fmt.Sscanf(version, "%d.%d", &major, &minor); err != nil || (major == 0 && minor < 8) {
		return "", fmt.Errorf("pgvector 0.8.0 or later required for iterative scans (found %s)", version)
	}
	return schema, nil
}

// Store implements vector.Store. K and G must be supported by database/sql.
type Store[K, G comparable] struct {
	db     *sql.DB
	schema Schema
	ext    string
}

// New attaches to installed tables without running DDL.
func New[K, G comparable](ctx context.Context, db *sql.DB, schema Schema) (*Store[K, G], error) {
	if err := schema.validate(); err != nil {
		return nil, err
	}
	if db == nil {
		return nil, errors.New("db is nil")
	}
	ext, err := extensionSchema(ctx, db)
	if err != nil {
		return nil, err
	}
	s := &Store[K, G]{db: db, schema: schema, ext: ext}
	if err := checkCatalog(ctx, db, schema); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Store[K, G]) generations() string { return quote(s.schema.Prefix + "_generations") }
func (s *Store[K, G]) stamps() string      { return quote(s.schema.Prefix + "_stamps") }
func (s *Store[K, G]) chunks(id int64) string {
	return quote(fmt.Sprintf("%s_chunks_g%d", s.schema.Prefix, id))
}
func (s *Store[K, G]) covered(doc, stamp string) string {
	return fmt.Sprintf(`(%s.doc_key IS NOT NULL AND %s.revision IS NOT DISTINCT FROM %s.%s)`, stamp, stamp, doc, quote(s.schema.RevisionColumn))
}

// checkCatalog checks the columns used by the store without reading any rows.
func checkCatalog(ctx context.Context, db *sql.DB, schema Schema) error {
	for _, table := range []struct{ name, columns string }{
		{schema.DocsTable, quote(schema.IDColumn) + "," + quote(schema.ContentColumn) + "," + quote(schema.RevisionColumn)},
		{schema.Prefix + "_generations", "id,gen_key,fingerprint,dimension,state"},
		{schema.Prefix + "_stamps", "generation_id,doc_key,revision"},
	} {
		rows, err := db.QueryContext(ctx, "SELECT "+table.columns+" FROM "+quote(table.name)+" WHERE false")
		if err != nil {
			return fmt.Errorf("check table %s: %w", table.name, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("check table %s: %w", table.name, err)
		}
	}
	return nil
}
