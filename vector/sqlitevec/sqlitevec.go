// Package sqlitevec implements vector.Store on top of SQLite with the
// sqlite-vec extension. It is a reference backend: a worked example of the
// storage contract the vector flows depend on, built against the same
// sqlite-vec binding msgvault uses.
//
// Callers must register the extension once before opening their database:
//
//	sqlitevec.Register()
//	db, _ := sql.Open("sqlite3", path)
//
// The caller owns the documents table; this package owns a small set of
// vector tables derived from VectorsPrefix. Each generation gets its own
// vec0 virtual table sized to that generation's dimension, so generations
// with different model dimensions coexist during a migration.
package sqlitevec

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"

	vecext "github.com/asg017/sqlite-vec-go-bindings/cgo"

	"go.kenn.io/kit/vector"
)

// Register loads the sqlite-vec extension into every SQLite connection
// opened afterwards in this process. It must be called before opening the
// database the store will use.
func Register() { vecext.Auto() }

var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// State is a generation's role in the active/building lifecycle. Only
// building and active generations are searched; building sorts ahead of
// active so Merge keeps the newer generation's hit on overlap.
type State string

const (
	StatePending  State = "pending"
	StateBuilding State = "building"
	StateActive   State = "active"
	StateRetired  State = "retired"
)

// Schema names the caller's documents table and the prefix for the
// vector tables this package manages. Every field must be a bare SQL
// identifier; values are validated before being interpolated into SQL.
type Schema struct {
	DocsTable      string // caller's documents table, e.g. "messages"
	IDColumn       string // primary key column, e.g. "id"
	ContentColumn  string // text to embed, e.g. "body"
	EmbedGenColumn string // nullable generation stamp, e.g. "embed_gen"
	VectorsPrefix  string // prefix for managed tables, e.g. "message_vectors"
}

func (s Schema) validate() error {
	for name, value := range map[string]string{
		"docs table":       s.DocsTable,
		"id column":        s.IDColumn,
		"content column":   s.ContentColumn,
		"embed gen column": s.EmbedGenColumn,
		"vectors prefix":   s.VectorsPrefix,
	} {
		if !identifierPattern.MatchString(value) {
			return fmt.Errorf("invalid %s %q", name, value)
		}
	}
	return nil
}

// Store implements vector.Store[K, G] against SQLite + sqlite-vec. K is the
// caller's document key type and G its generation key type; both must be
// types database/sql can bind and scan (for example int64 or string).
type Store[K, G comparable] struct {
	db     *sql.DB
	schema Schema
}

// New returns a Store bound to db. The caller retains ownership of db. New
// creates the generations and chunks bookkeeping tables if they do not
// exist; per-generation vec0 tables are created by EnsureGeneration.
func New[K, G comparable](ctx context.Context, db *sql.DB, schema Schema) (*Store[K, G], error) {
	if err := schema.validate(); err != nil {
		return nil, err
	}
	if db == nil {
		return nil, fmt.Errorf("db is nil")
	}
	s := &Store[K, G]{db: db, schema: schema}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
    ordinal   INTEGER PRIMARY KEY,
    gen_key   UNIQUE,
    dimension INTEGER NOT NULL,
    state     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS %s (
    ordinal     INTEGER NOT NULL,
    doc_key     NOT NULL,
    chunk_index INTEGER NOT NULL,
    vec_rowid   INTEGER NOT NULL,
    PRIMARY KEY (ordinal, doc_key, chunk_index)
);`, s.generationsTable(), s.chunksTable())); err != nil {
		return nil, fmt.Errorf("create bookkeeping tables: %w", err)
	}
	return s, nil
}

func (s *Store[K, G]) generationsTable() string { return s.schema.VectorsPrefix + "_generations" }
func (s *Store[K, G]) chunksTable() string      { return s.schema.VectorsPrefix + "_chunks" }
func (s *Store[K, G]) vecTable(ordinal int64) string {
	return fmt.Sprintf("%s_v%d", s.schema.VectorsPrefix, ordinal)
}

// EnsureGeneration registers gen with model's dimension and the given
// state, creating its vec0 table on first use. Calling it again updates
// only the state; a generation's dimension is fixed once created.
func (s *Store[K, G]) EnsureGeneration(ctx context.Context, gen G, model vector.Generation, state State) error {
	if model.Dimensions <= 0 {
		return fmt.Errorf("generation dimension must be positive, got %d", model.Dimensions)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ensure generation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO %s (gen_key, dimension, state) VALUES (?, ?, ?)
ON CONFLICT(gen_key) DO UPDATE SET state = excluded.state`, s.generationsTable()),
		gen, model.Dimensions, string(state)); err != nil {
		return fmt.Errorf("upsert generation: %w", err)
	}

	ordinal, dimension, err := s.lookupGenerationTx(ctx, tx, gen)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`CREATE VIRTUAL TABLE IF NOT EXISTS %s USING vec0(embedding float[%d] distance_metric=cosine)`,
		s.vecTable(ordinal), dimension)); err != nil {
		return fmt.Errorf("create vec0 table: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ensure generation: %w", err)
	}
	return nil
}

// SetGenerationState transitions gen to state. The caller owns the
// active/building lifecycle; this only records the decision.
func (s *Store[K, G]) SetGenerationState(ctx context.Context, gen G, state State) error {
	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET state = ? WHERE gen_key = ?`, s.generationsTable()),
		string(state), gen)
	if err != nil {
		return fmt.Errorf("set generation state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("generation %v not found", gen)
	}
	return nil
}

func (s *Store[K, G]) lookupGeneration(ctx context.Context, gen G) (ordinal int64, dimension int, err error) {
	return s.scanGeneration(s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT ordinal, dimension FROM %s WHERE gen_key = ?`, s.generationsTable()), gen), gen)
}

func (s *Store[K, G]) lookupGenerationTx(ctx context.Context, tx *sql.Tx, gen G) (int64, int, error) {
	return s.scanGeneration(tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT ordinal, dimension FROM %s WHERE gen_key = ?`, s.generationsTable()), gen), gen)
}

func (s *Store[K, G]) scanGeneration(row *sql.Row, gen G) (int64, int, error) {
	var ordinal int64
	var dimension int
	if err := row.Scan(&ordinal, &dimension); err != nil {
		if err == sql.ErrNoRows {
			return 0, 0, fmt.Errorf("generation %v not ensured", gen)
		}
		return 0, 0, fmt.Errorf("lookup generation %v: %w", gen, err)
	}
	return ordinal, dimension, nil
}
