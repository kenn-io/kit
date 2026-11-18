// Package pgvector implements vector.Store using PostgreSQL and pgvector.
// The caller owns the database connection and documents table.
package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
)

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
		if !identifier.MatchString(value) {
			return fmt.Errorf("invalid %s %q", name, value)
		}
	}
	return nil
}
func quote(s string) string { return `"` + s + `"` }

// ErrPgvectorMissing means the database has no pgvector halfvec type.
var ErrPgvectorMissing = errors.New("pgvector halfvec is unavailable")

func extensionSchema(ctx context.Context, db *sql.DB) (string, error) {
	var schema string
	err := db.QueryRowContext(ctx, `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace JOIN pg_type t ON t.typnamespace=n.oid AND t.typname='halfvec' WHERE e.extname='vector'`).Scan(&schema)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrPgvectorMissing
	}
	if err != nil {
		return "", fmt.Errorf("find pgvector: %w", err)
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
	s := &Store[K, G]{db: db, schema: schema, ext: quote(ext)}
	for _, name := range []string{s.generations(), s.stamps()} {
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("table %s is missing", name)
		}
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
