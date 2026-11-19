// Package pgvector implements vector.Store using PostgreSQL and pgvector.
// The caller owns the database connection and documents table. RevisionColumn
// must change whenever document content changes, including when a deleted
// document key is reused for replacement content. Retire and reclaim generations
// through Store methods so they coordinate with in-flight saves. SaveDocuments
// requires READ COMMITTED, schema USAGE, SELECT on generations, SELECT and
// UPDATE on documents, SELECT, INSERT, UPDATE, and DELETE on stamps, and SELECT,
// INSERT, and DELETE on chunks. Its locks last until the caller commits or rolls
// back. A caller deleting a document before saving may deadlock with Reclaim;
// PostgreSQL aborts one side.
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
	DocsTable      string
	IDColumn       string
	ContentColumn  string
	RevisionColumn string
	Prefix         string
	// SharedChunks uses a caller-installed {prefix}_chunks table with generation_id
	// bigint, doc_key matching the document key type and referencing it with
	// ON DELETE CASCADE, chunk_index integer or bigint, and embedding unbounded
	// halfvec. Its primary key is (generation_id, doc_key, chunk_index).
	// Without a caller-installed vector index, shared searches are exact. Kit
	// creates or tunes no vector index on this table; callers own any index they
	// add and its search behavior. Without the cascade, deleted documents' vectors
	// remain in storage but are never returned. The default uses a chunk table
	// and HNSW index per generation.
	SharedChunks bool
	// Begin opens write transactions; the store commits or rolls them back.
	// If nil, the store opens transactions on the database pool.
	Begin func(context.Context, *sql.TxOptions) (*sql.Tx, error)
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

func extensionSchema(ctx context.Context, db *sql.DB, shared bool) (string, error) {
	var schema, version string
	err := db.QueryRowContext(ctx, `SELECT n.nspname, e.extversion FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='vector'`).Scan(&schema, &version)
	if err != nil {
		return "", fmt.Errorf("find pgvector: %w", err)
	}
	if !shared && !supportedVersion(version) {
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

// Store implements vector.Store. K must be supported by database/sql; generation
// keys are stored as text.
type Store[K comparable, G ~string] struct {
	db     *sql.DB
	schema Schema
	ext    string
}

// New attaches to installed tables without running DDL.
func New[K comparable, G ~string](ctx context.Context, db *sql.DB, schema Schema) (*Store[K, G], error) {
	if err := schema.validate(); err != nil {
		return nil, err
	}
	if db == nil {
		return nil, errors.New("db is nil")
	}
	ext, err := extensionSchema(ctx, db, schema.SharedChunks)
	if err != nil {
		return nil, err
	}
	s := &Store[K, G]{db: db, schema: schema, ext: ext}
	if err := checkCatalog(ctx, db, schema); err != nil {
		return nil, err
	}
	if schema.SharedChunks {
		name := schema.Prefix + "_chunks"
		if err := checkTable(ctx, db, name, "generation_id,doc_key,chunk_index,embedding"); err != nil {
			return nil, fmt.Errorf("check table %s: %w", name, err)
		}
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

func (s *Store[K, G]) chunkTable(id int64) string {
	if s.schema.SharedChunks {
		return postgres.QuoteIdentifier(s.schema.Prefix + "_chunks")
	}
	return s.chunks(id)
}

func (s *Store[K, G]) begin(ctx context.Context) (*sql.Tx, error) {
	return s.beginWithOptions(ctx, nil)
}

func (s *Store[K, G]) beginWithOptions(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	if s.schema.Begin != nil {
		return s.schema.Begin(ctx, opts)
	}
	return s.db.BeginTx(ctx, opts)
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
