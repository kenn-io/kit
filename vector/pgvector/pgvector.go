// Package pgvector implements vector.Store using PostgreSQL and pgvector.
// The caller owns the database connection and documents table. RevisionColumn
// must change whenever document content changes, including when a deleted
// document key is reused for replacement content.
package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/kit/search/postgres"
)

// Schema names the source documents and managed tables.
type Schema struct {
	DocsTable         string
	IDColumn          string
	ContentColumn     string
	RevisionColumn    string
	Prefix            string
	GenerationKeyType string // PostgreSQL type for G; defaults to text.
}

func (s *Schema) validate() error {
	for name, value := range map[string]string{"docs table": s.DocsTable, "id column": s.IDColumn, "content column": s.ContentColumn, "revision column": s.RevisionColumn, "prefix": s.Prefix} {
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

func extensionSchema(ctx context.Context, db *sql.DB) (string, error) {
	var schema, version string
	err := db.QueryRowContext(ctx, `SELECT n.nspname, e.extversion FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='vector'`).Scan(&schema, &version)
	if err != nil {
		return "", fmt.Errorf("find pgvector: %w", err)
	}
	if !supportedVersion(version) {
		return "", fmt.Errorf("pgvector 0.8.0 or later required for iterative scans (found %s)", version)
	}
	return schema, nil
}

// supportedVersion accepts numeric three-part versions at or above 0.8.0.
func supportedVersion(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	var numbers [3]uint64
	for i, part := range parts {
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return false
		}
		numbers[i] = n
	}
	return numbers[0] > 0 || numbers[1] >= 8
}

// Store implements vector.Store. K and G must be supported by database/sql.
// G uses Schema.GenerationKeyType, which defaults to text.
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

func (s *Store[K, G]) generations() string {
	return postgres.QuoteIdentifier(s.schema.Prefix + "_generations")
}

func (s *Store[K, G]) stamps() string { return postgres.QuoteIdentifier(s.schema.Prefix + "_stamps") }

func (s *Store[K, G]) chunks(id int64) string {
	return postgres.QuoteIdentifier(fmt.Sprintf("%s_chunks_g%d", s.schema.Prefix, id))
}

func (s *Store[K, G]) covered(doc, stamp string) string {
	return fmt.Sprintf(`(%s.doc_key IS NOT NULL AND %s.revision IS NOT DISTINCT FROM %s.%s)`, stamp, stamp, doc, postgres.QuoteIdentifier(s.schema.RevisionColumn))
}

// checkCatalog checks the columns used by the store without reading any rows.
func checkCatalog(ctx context.Context, db *sql.DB, schema Schema) error {
	for _, table := range []struct{ name, columns string }{
		{schema.DocsTable, postgres.QuoteIdentifier(schema.IDColumn) + "," + postgres.QuoteIdentifier(schema.ContentColumn) + "," + postgres.QuoteIdentifier(schema.RevisionColumn)},
		{schema.Prefix + "_generations", "id,gen_key,fingerprint,dimension,state"},
		{schema.Prefix + "_stamps", "generation_id,doc_key,revision"},
	} {
		if err := checkTable(ctx, db, table.name, table.columns); err != nil {
			return fmt.Errorf("check table %s: %w", table.name, err)
		}
	}
	return nil
}

func checkTable(ctx context.Context, db *sql.DB, name, columns string) error {
	rows, err := db.QueryContext(ctx, "SELECT "+columns+" FROM "+postgres.QuoteIdentifier(name)+" WHERE false")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	return rows.Err()
}
