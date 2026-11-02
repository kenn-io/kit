package sqlitevec_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

const rowsErrDriverName = "sqlitevec_rows_err"

var rowsErrDeleteCount atomic.Int64

func init() {
	sql.Register(rowsErrDriverName, rowsErrDriver{})
}

type rowsErrDriver struct{}

func (rowsErrDriver) Open(_ string) (driver.Conn, error) {
	return rowsErrConn{}, nil
}

type rowsErrConn struct{}

func (rowsErrConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, errors.New("prepare not implemented")
}

func (rowsErrConn) Close() error {
	return nil
}

func (rowsErrConn) Begin() (driver.Tx, error) {
	return rowsErrTx{}, nil
}

func (rowsErrConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return rowsErrTx{}, nil
}

func (rowsErrConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(query, "DELETE FROM") {
		rowsErrDeleteCount.Add(1)
	}
	return driver.RowsAffected(0), nil
}

func (rowsErrConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	return &rowsErrRows{indexCheck: strings.Contains(query, "sqlite_master")}, nil
}

type rowsErrTx struct{}

func (rowsErrTx) Commit() error {
	return nil
}

func (rowsErrTx) Rollback() error {
	return nil
}

type rowsErrRows struct {
	returned   bool
	indexCheck bool
}

func (rows *rowsErrRows) Columns() []string {
	if rows.indexCheck {
		return []string{"indexes_exist"}
	}
	return []string{"ordinal", "vec_rowid"}
}

func (rows *rowsErrRows) Close() error {
	return nil
}

func (rows *rowsErrRows) Next(dest []driver.Value) error {
	if !rows.returned {
		rows.returned = true
		if rows.indexCheck {
			dest[0] = int64(1)
			return nil
		}
		dest[0] = int64(1)
		dest[1] = int64(42)
		return nil
	}
	return io.ErrUnexpectedEOF
}

// topicEncoder maps text to a one-hot 3-D vector by keyword, so queries
// match documents deterministically.
func topicEncoder() vector.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			switch {
			case strings.Contains(text, "cat"):
				out[i] = []float32{1, 0, 0}
			case strings.Contains(text, "dog"):
				out[i] = []float32{0, 1, 0}
			default:
				out[i] = []float32{0, 0, 1}
			}
		}
		return out, nil
	}
}

func setup(t *testing.T) (*sql.DB, *sqlitevec.Store[int64, int64]) {
	t.Helper()
	require := require.New(t)
	t.Helper()
	db, err := openSQLiteTestDB(t, filepath.Join(t.TempDir(), "vec.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(db.Close()) })

	_, err = db.ExecContext(t.Context(), `CREATE TABLE messages (id INTEGER PRIMARY KEY, body TEXT, embed_gen INTEGER)`)
	require.NoError(err)

	store, err := sqlitevec.New[int64, int64](t.Context(), db, sqlitevec.Schema{
		DocsTable:      "messages",
		IDColumn:       "id",
		ContentColumn:  "body",
		EmbedGenColumn: "embed_gen",
		VectorsPrefix:  "message_vectors",
	})
	require.NoError(err)
	return db, store
}

func TestStoreFillThenSearch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, 'a cat sat'), (2, 'a dog ran')`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	stats, err := vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(err)
	assert.Equal(2, stats.Documents)

	pending, err := store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	assert.Empty(pending, "nothing pending once every document is stamped")

	enc := func(int64) vector.EncodeFunc { return topicEncoder() }
	hits, err := vector.Search(ctx, store, "a cat", enc, vector.SearchOptions{})
	require.NoError(err)
	require.NotEmpty(hits)
	assert.Equal(int64(1), hits[0].Doc, "the cat query ranks the cat document first")
}

func TestStoreReembeddingReplacesVectors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, 'a cat sat')`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	require.NoError(store.SaveVectors(ctx, 1, 1, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))
	require.NoError(store.SaveVectors(ctx, 1, 1, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{0, 1, 0}}}))

	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{0, 1, 0}, 10)
	require.NoError(err)
	require.Len(hits, 1, "re-embedding replaces the prior vector rather than duplicating it")
	assert.InDelta(1.0, hits[0].Score, 1e-6, "stored vector now matches the new query")
}

func TestStoreSearchUnionsLiveGenerations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, 'a cat'), (2, 'a dog')`)
	require.NoError(err)

	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "v1", Dimensions: 3}, sqlitevec.StateActive))
	_, err = vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(err)

	// The building generation has covered only doc 1 so far.
	require.NoError(store.EnsureGeneration(ctx, 2, vector.Generation{Model: "v2", Dimensions: 3}, sqlitevec.StateBuilding))
	require.NoError(store.SaveVectors(ctx, 2, 1, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))

	gens, err := store.LiveGenerations(ctx)
	require.NoError(err)
	assert.Equal([]int64{2, 1}, gens, "building precedes active in preference order")

	enc := func(int64) vector.EncodeFunc { return topicEncoder() }
	hits, err := vector.Search(ctx, store, "a cat", enc, vector.SearchOptions{})
	require.NoError(err)

	found := map[int64]bool{}
	for _, h := range hits {
		found[h.Doc] = true
	}
	assert.True(found[1], "shared doc is searchable")
	assert.True(found[2], "active-only doc is not dropped while generations overlap")
}

func TestStorePendingForGenerationUsesPerGenerationStampCoverage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, 'a cat')`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "v1", Dimensions: 3}, sqlitevec.StateActive))
	require.NoError(store.EnsureGeneration(ctx, 2, vector.Generation{Model: "v2", Dimensions: 3}, sqlitevec.StateBuilding))
	require.NoError(store.SaveVectors(ctx, 1, 1, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))
	require.NoError(store.SaveVectors(ctx, 2, 1, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))

	pending, err := store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	assert.Empty(pending, "an older live generation with its own stamp is not refilled after the doc is stamped for a newer generation")
}

func TestStoreSaveVectorsClearsAllGenerationsAfterNonRevisionInvalidation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, 'a cat')`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "v1", Dimensions: 3}, sqlitevec.StateActive))
	require.NoError(store.EnsureGeneration(ctx, 2, vector.Generation{Model: "v2", Dimensions: 3}, sqlitevec.StateBuilding))
	require.NoError(store.SaveVectors(ctx, 1, 1, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))
	require.NoError(store.SaveVectors(ctx, 2, 1, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))

	_, err = db.ExecContext(ctx, `UPDATE messages SET body = 'a dog', embed_gen = NULL WHERE id = 1`)
	require.NoError(err)
	pending, err := store.PendingForGeneration(ctx, 2, 10)
	require.NoError(err)
	require.Len(pending, 1)
	require.NoError(store.SaveVectors(ctx, 2, pending[0].Doc, nil,
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{0, 1, 0}}}))

	pending, err = store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	require.Len(pending, 1, "the older generation stays pending after another generation refreshes an invalidated row")
	assert.Equal("a dog", pending[0].Content)

	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{1, 0, 0}, 10)
	require.NoError(err)
	assert.Empty(hits, "stale older-generation vectors are removed after document-wide invalidation")

	hits, err = store.QueryGeneration(ctx, 2, vector.Vector{0, 1, 0}, 10)
	require.NoError(err)
	require.Len(hits, 1)
	assert.Equal(int64(1), hits[0].Doc)
}

func TestStoreEnsureGenerationRejectsChangedFingerprint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setup(t)

	require.NoError(store.EnsureGeneration(ctx, 1,
		vector.Generation{Model: "m1", Dimensions: 3, Params: map[string]string{"pooling": "mean"}},
		sqlitevec.StateBuilding))

	err := store.EnsureGeneration(ctx, 1,
		vector.Generation{Model: "m2", Dimensions: 3, Params: map[string]string{"pooling": "mean"}},
		sqlitevec.StateActive)
	require.ErrorContains(err, "different model")

	var state string
	err = db.QueryRowContext(ctx, `SELECT state FROM message_vectors_generations WHERE gen_key = ?`, 1).Scan(&state)
	require.NoError(err)
	assert.Equal(string(sqlitevec.StateBuilding), state, "mismatched ensure does not change the existing generation state")
}

func TestNewCreatesChunkLookupIndexes(t *testing.T) {
	assert := assert.New(t)
	db, store := setup(t)
	_, err := db.ExecContext(t.Context(), `INSERT INTO messages (id, body) VALUES (1, 'cat')`)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(t.Context(), 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	require.NoError(t, store.SaveVectors(t.Context(), 1, 1, nil, nil))

	assert.Equal([]string{"ordinal", "vec_rowid"}, indexColumns(t, db, "message_vectors_chunks_by_vector"))
	assert.Equal([]string{"doc_key", "ordinal", "vec_rowid"}, indexColumns(t, db, "message_vectors_chunks_by_doc"))
	assert.Equal([]string{"doc_key", "revision"}, indexColumns(t, db, "message_vectors_stamps_by_doc_revision"))
	assert.Equal([]string{"ordinal", "", "doc_key"}, indexColumns(t, db, "message_vectors_stamps_by_doc_numeric"))
	assert.Equal([]string{"ordinal", "", "doc_key"}, indexColumns(t, db, "message_vectors_chunks_by_doc_numeric"))
}

func TestNewReadOnlyWithoutNumericIndexes(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "vec.db")
	db, err := openSQLiteTestDB(t, path)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(ctx, `CREATE TABLE messages (id INTEGER PRIMARY KEY, body TEXT, embed_gen INTEGER);
INSERT INTO messages (id, body) VALUES (1, 'cat')`)
	require.NoError(t, err)
	schema := sqlitevec.Schema{
		DocsTable: "messages", IDColumn: "id", ContentColumn: "body", EmbedGenColumn: "embed_gen", VectorsPrefix: "message_vectors",
	}
	store, err := sqlitevec.New[int64, int64](ctx, db, schema)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	_, err = vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DROP INDEX message_vectors_stamps_by_doc_numeric; DROP INDEX message_vectors_chunks_by_doc_numeric`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	for _, mode := range []string{"mode_ro", "query_only"} {
		t.Run(mode, func(t *testing.T) {
			dsn := path
			if mode == "mode_ro" {
				uriPath := filepath.ToSlash(path)
				if !strings.HasPrefix(uriPath, "/") {
					uriPath = "/" + uriPath
				}
				dsn = (&url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro"}).String()
			}
			reader, err := openSQLiteTestDB(t, dsn)
			require.NoError(t, err)
			defer func() { require.NoError(t, reader.Close()) }()
			reader.SetMaxOpenConns(1)
			if mode == "query_only" {
				_, err = reader.ExecContext(ctx, `PRAGMA query_only = ON`)
				require.NoError(t, err)
			}
			readStore, err := sqlitevec.New[int64, int64](ctx, reader, schema)
			require.NoError(t, err)
			coverage, err := readStore.Coverage(ctx, 1, "")
			require.NoError(t, err)
			assert.Equal(t, sqlitevec.Coverage{Embedded: 1}, coverage)
		})
	}

	writer, err := openSQLiteTestDB(t, path)
	require.NoError(t, err)
	defer func() { require.NoError(t, writer.Close()) }()
	upgraded, err := sqlitevec.New[int64, int64](ctx, writer, schema)
	require.NoError(t, err)
	require.NoError(t, upgraded.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	assert.Equal(t, []string{"ordinal", "", "doc_key"}, indexColumns(t, writer, "message_vectors_stamps_by_doc_numeric"))
	assert.Equal(t, []string{"ordinal", "", "doc_key"}, indexColumns(t, writer, "message_vectors_chunks_by_doc_numeric"))
}

func indexColumns(t *testing.T, db *sql.DB, index string) []string {
	t.Helper()
	require := require.New(t)
	t.Helper()
	rows, err := db.QueryContext(t.Context(), fmt.Sprintf(`PRAGMA index_info(%s)`, index))
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()

	var columns []string
	for rows.Next() {
		var seqno, cid int
		var name sql.NullString
		require.NoError(rows.Scan(&seqno, &cid, &name))
		columns = append(columns, name.String)
	}
	require.NoError(rows.Err())
	return columns
}

func TestDocKeyMatchPreservesSQLiteAffinityMatches(t *testing.T) {
	for _, tc := range []struct{ name, declaration, suffix string }{
		{"integer", "INTEGER PRIMARY KEY", ""},
		{"text", "TEXT", ""},
		{"real", "REAL", ""},
		{"numeric", "NUMERIC", ""},
		{"untyped", "", ""},
		{"strict_any", "ANY", "STRICT"},
		{"nocase", "NUMERIC COLLATE NOCASE", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			db, _ := setup(t)
			_, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE docs (id %s, body TEXT, embed_gen INTEGER) %s`, tc.declaration, tc.suffix))
			require.NoError(t, err)
			store, err := sqlitevec.New[string, int64](ctx, db, sqlitevec.Schema{
				DocsTable: "docs", IDColumn: "id", ContentColumn: "body", EmbedGenColumn: "embed_gen", VectorsPrefix: "test_vectors",
			})
			require.NoError(t, err)
			keys := []any{int64(1), "1", " 1", "01", "001", "1.0", float64(4), "abc", "ABC", int64(-9223372036854775808), int64(9223372036854775807), "-9223372036854775808", "9223372036854775807"}
			ids := keys
			if tc.name == "integer" {
				ids = []any{int64(1), int64(4), int64(-9223372036854775808), int64(9223372036854775807)}
			}
			for _, id := range ids {
				_, err := db.ExecContext(ctx, `INSERT INTO docs (id) VALUES (?)`, id)
				require.NoError(t, err)
			}
			for _, key := range keys {
				_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO test_vectors_stamps (ordinal, doc_key) VALUES (1, ?)`, key)
				require.NoError(t, err)
			}
			var pairs [2][][2]int64
			for i, query := range []string{
				`SELECT d.rowid, stamp.rowid FROM docs d LEFT JOIN test_vectors_stamps stamp ON stamp.ordinal = ? AND stamp.doc_key = d.id`,
				store.PendingQueryForTest("d.rowid, stamp.rowid"),
			} {
				args := []any{1}
				if i == 1 {
					args = append(args, -1)
				}
				func() {
					rows, err := db.QueryContext(ctx, query, args...)
					require.NoError(t, err)
					defer func() { require.NoError(t, rows.Close()) }()
					for rows.Next() {
						var doc int64
						var stamp sql.NullInt64
						require.NoError(t, rows.Scan(&doc, &stamp))
						if stamp.Valid {
							pairs[i] = append(pairs[i], [2]int64{doc, stamp.Int64})
						}
					}
					require.NoError(t, rows.Err())
				}()
			}
			require.NotEmpty(t, pairs[0])
			assert.ElementsMatch(t, pairs[0], pairs[1])
		})
	}
}

func TestStoreSaveVectorsRejectsMissingDocument(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	_, store := setup(t)

	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	err := store.SaveVectors(ctx, 1, 999, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}})
	require.Error(err, "saving vectors for a document not in the source table fails")

	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{1, 0, 0}, 10)
	require.NoError(err)
	assert.Empty(hits, "no orphan vectors are committed when the source row is missing")
}

// setupWithRevision mirrors setup but adds a last_modified revision column
// so SaveVectors stamps optimistically.
func setupWithRevision(t *testing.T) (*sql.DB, *sqlitevec.Store[int64, int64]) {
	t.Helper()
	require := require.New(t)
	t.Helper()
	db, err := openSQLiteTestDB(t, filepath.Join(t.TempDir(), "vec.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(db.Close()) })

	_, err = db.ExecContext(t.Context(), `CREATE TABLE messages (
		id INTEGER PRIMARY KEY, body TEXT, embed_gen INTEGER,
		last_modified INTEGER NOT NULL DEFAULT 0)`)
	require.NoError(err)

	store, err := sqlitevec.New[int64, int64](t.Context(), db, sqlitevec.Schema{
		DocsTable:      "messages",
		IDColumn:       "id",
		ContentColumn:  "body",
		EmbedGenColumn: "embed_gen",
		RevisionColumn: "last_modified",
		VectorsPrefix:  "message_vectors",
	})
	require.NoError(err)
	return db, store
}

func TestStoreStaleRevisionLeavesDocumentPending(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setupWithRevision(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat sat', 1)`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	pending, err := store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	require.Len(pending, 1)

	// A concurrent edit bumps the revision between scan and save.
	_, err = db.ExecContext(ctx, `UPDATE messages SET last_modified = 2 WHERE id = 1`)
	require.NoError(err)

	err = store.SaveVectors(ctx, 1, pending[0].Doc, pending[0].Revision,
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}})
	require.ErrorIs(err, vector.ErrStale)

	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{1, 0, 0}, 10)
	require.NoError(err)
	assert.Empty(hits, "a stale save commits no vectors")

	pending, err = store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	require.Len(pending, 1, "the changed document stays pending")

	// A retry with the fresh revision succeeds and drains pending.
	require.NoError(store.SaveVectors(ctx, 1, pending[0].Doc, pending[0].Revision,
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))
	pending, err = store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	assert.Empty(pending)
}

func TestStorePendingForGenerationDetectsRevisionEditAfterStamp(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setupWithRevision(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat sat', 1)`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	pending, err := store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	require.Len(pending, 1)
	require.NoError(store.SaveVectors(ctx, 1, pending[0].Doc, pending[0].Revision,
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))

	pending, err = store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	require.Empty(pending)

	_, err = db.ExecContext(ctx, `UPDATE messages SET body = 'a cat jumped', last_modified = 2 WHERE id = 1`)
	require.NoError(err)

	pending, err = store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	require.Len(pending, 1, "a revision-only edit makes the document pending again")
	assert.Equal(int64(1), pending[0].Doc)
	assert.Equal("a cat jumped", pending[0].Content)
	assert.Equal(int64(2), pending[0].Revision)
}

func TestStoreSaveVectorsStoresRevisionAfterStampTriggers(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	db, store := setupWithRevision(t)

	_, err := db.ExecContext(ctx, `
CREATE TRIGGER bump_revision_after_embed_stamp
AFTER UPDATE OF embed_gen ON messages
BEGIN
    UPDATE messages SET last_modified = last_modified + 1 WHERE id = NEW.id;
END;
INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat sat', 1);`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	pending, err := store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	require.Len(pending, 1)
	require.NoError(store.SaveVectors(ctx, 1, pending[0].Doc, pending[0].Revision,
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))

	pending, err = store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	require.Empty(pending, "the stamp stores the revision after embed_gen triggers run")
}

func TestStoreSaveVectorsAdvancesCoveredGenerationStampsAfterStampTriggers(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	db, store := setupWithRevision(t)

	_, err := db.ExecContext(ctx, `
CREATE TRIGGER bump_revision_after_embed_stamp
AFTER UPDATE OF embed_gen ON messages
BEGIN
    UPDATE messages SET last_modified = last_modified + 1 WHERE id = NEW.id;
END;
INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat sat', 1);`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "v1", Dimensions: 3}, sqlitevec.StateActive))
	require.NoError(store.EnsureGeneration(ctx, 2, vector.Generation{Model: "v2", Dimensions: 3}, sqlitevec.StateBuilding))

	pending, err := store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	require.Len(pending, 1)
	require.NoError(store.SaveVectors(ctx, 1, pending[0].Doc, pending[0].Revision,
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))

	pending, err = store.PendingForGeneration(ctx, 2, 10)
	require.NoError(err)
	require.Len(pending, 1)
	require.NoError(store.SaveVectors(ctx, 2, pending[0].Doc, pending[0].Revision,
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))

	pending, err = store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	require.Empty(pending, "saving another generation with a stamp-only revision bump does not stale covered generations")
}

func TestStoreSaveVectorsClearsAllGenerationsAfterRevisionInvalidation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setupWithRevision(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat', 1)`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "v1", Dimensions: 3}, sqlitevec.StateActive))
	require.NoError(store.EnsureGeneration(ctx, 2, vector.Generation{Model: "v2", Dimensions: 3}, sqlitevec.StateBuilding))
	require.NoError(store.SaveVectors(ctx, 1, 1, int64(1), []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))
	require.NoError(store.SaveVectors(ctx, 2, 1, int64(1), []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))

	_, err = db.ExecContext(ctx, `UPDATE messages SET body = 'a dog', last_modified = 2, embed_gen = NULL WHERE id = 1`)
	require.NoError(err)
	pending, err := store.PendingForGeneration(ctx, 2, 10)
	require.NoError(err)
	require.Len(pending, 1)
	require.NoError(store.SaveVectors(ctx, 2, pending[0].Doc, pending[0].Revision,
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{0, 1, 0}}}))

	pending, err = store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	require.Len(pending, 1, "the older generation stays pending after another generation refreshes an invalidated revisioned row")
	assert.Equal("a dog", pending[0].Content)
	assert.Equal(int64(2), pending[0].Revision)

	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{1, 0, 0}, 10)
	require.NoError(err)
	assert.Empty(hits, "stale older-generation vectors are removed after revisioned document-wide invalidation")

	hits, err = store.QueryGeneration(ctx, 2, vector.Vector{0, 1, 0}, 10)
	require.NoError(err)
	require.Len(hits, 1)
	assert.Equal(int64(1), hits[0].Doc)
}

func TestStoreSaveVectorsClearsAllGenerationsAfterRevisionOnlyEdit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setupWithRevision(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat', 1)`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "v1", Dimensions: 3}, sqlitevec.StateActive))
	require.NoError(store.EnsureGeneration(ctx, 2, vector.Generation{Model: "v2", Dimensions: 3}, sqlitevec.StateBuilding))
	require.NoError(store.SaveVectors(ctx, 1, 1, int64(1), []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))
	require.NoError(store.SaveVectors(ctx, 2, 1, int64(1), []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))

	_, err = db.ExecContext(ctx, `UPDATE messages SET body = 'a dog', last_modified = 2 WHERE id = 1`)
	require.NoError(err)
	pending, err := store.PendingForGeneration(ctx, 2, 10)
	require.NoError(err)
	require.Len(pending, 1)
	require.NoError(store.SaveVectors(ctx, 2, pending[0].Doc, pending[0].Revision,
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{0, 1, 0}}}))

	pending, err = store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	require.Len(pending, 1, "the older generation stays pending after another generation refreshes a changed row")
	assert.Equal("a dog", pending[0].Content)
	assert.Equal(int64(2), pending[0].Revision)

	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{1, 0, 0}, 10)
	require.NoError(err)
	assert.Empty(hits, "stale older-generation vectors are removed after a revision-only edit")

	hits, err = store.QueryGeneration(ctx, 2, vector.Vector{0, 1, 0}, 10)
	require.NoError(err)
	require.Len(hits, 1)
	assert.Equal(int64(1), hits[0].Doc)
}

func TestStoreFillWithRevisionColumn(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setupWithRevision(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat', 3), (2, 'a dog', 4)`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	stats, err := vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(err)
	assert.Equal(2, stats.Documents)

	pending, err := store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	assert.Empty(pending)
}

func TestStorePendingForGenerationTreatsNullContentAsEmpty(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, NULL)`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	pending, err := store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal(int64(1), pending[0].Doc)
	assert.Empty(pending[0].Content)
}

func TestStoreSaveVectorsRevisionRequiresColumn(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, 'a cat')`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	err = store.SaveVectors(ctx, 1, 1, int64(5),
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}})
	require.ErrorContains(err, "revision")
}

func TestStoreStampOnlySaveDropsDocumentFromPending(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, 'a cat')`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	require.NoError(store.SaveVectors(ctx, 1, 1, nil, nil), "an empty save stamps without vectors")

	pending, err := store.PendingForGeneration(ctx, 1, 10)
	require.NoError(err)
	assert.Empty(pending, "a stamp-only document stops being pending")

	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{1, 0, 0}, 10)
	require.NoError(err)
	assert.Empty(hits)
}

func TestStoreQueryGenerationExcludesDeletedDocuments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, 'a cat sat'), (2, 'a dog ran')`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	_, err = vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(err)

	// The caller deletes a source row without telling the store.
	_, err = db.ExecContext(ctx, `DELETE FROM messages WHERE id = 1`)
	require.NoError(err)

	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{1, 0, 0}, 10)
	require.NoError(err)
	docs := map[int64]bool{}
	for _, h := range hits {
		docs[h.Doc] = true
	}
	assert.False(docs[1], "a deleted document's vectors are not returned as hits")
	assert.True(docs[2], "surviving documents still match")
}

func TestStoreQueryGenerationExcludesEditedDocumentUntilReembedded(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setupWithRevision(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat sat', 1)`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	_, err = vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(err)

	// The caller redacts the content, bumping the revision.
	_, err = db.ExecContext(ctx, `UPDATE messages SET body = 'a dog ran', last_modified = 2 WHERE id = 1`)
	require.NoError(err)

	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{1, 0, 0}, 10)
	require.NoError(err)
	assert.Empty(hits, "the pre-edit vector never surfaces after the revision changes")

	_, err = vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(err)

	hits, err = store.QueryGeneration(ctx, 1, vector.Vector{0, 1, 0}, 10)
	require.NoError(err)
	require.Len(hits, 1, "the re-embedded content is searchable again")
	assert.Equal(int64(1), hits[0].Doc)
}

func TestStoreQueryGenerationExcludesInvalidatedDocumentUntilReembedded(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, 'a cat sat')`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	_, err = vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(err)

	// Without a revision column, the caller signals an edit by clearing
	// the embed-gen stamp.
	_, err = db.ExecContext(ctx, `UPDATE messages SET body = 'a dog ran', embed_gen = NULL WHERE id = 1`)
	require.NoError(err)

	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{1, 0, 0}, 10)
	require.NoError(err)
	assert.Empty(hits, "the pre-invalidation vector never surfaces while the document is pending")

	_, err = vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(err)

	hits, err = store.QueryGeneration(ctx, 1, vector.Vector{0, 1, 0}, 10)
	require.NoError(err)
	require.Len(hits, 1, "the re-embedded content is searchable again")
	assert.Equal(int64(1), hits[0].Doc)
}

func TestStoreDeleteVectorsRemovesAllGenerations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, 'a cat')`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "v1", Dimensions: 3}, sqlitevec.StateActive))
	require.NoError(store.EnsureGeneration(ctx, 2, vector.Generation{Model: "v2", Dimensions: 3}, sqlitevec.StateBuilding))
	require.NoError(store.SaveVectors(ctx, 1, 1, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))
	require.NoError(store.SaveVectors(ctx, 2, 1, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0, 0}}}))

	require.NoError(store.DeleteVectors(ctx, 1))

	for _, gen := range []int64{1, 2} {
		hits, err := store.QueryGeneration(ctx, gen, vector.Vector{1, 0, 0}, 10)
		require.NoError(err)
		assert.Empty(hits, "generation %d holds no vectors after delete", gen)
	}

	// Deleting a document with no vectors is a no-op, not an error.
	require.NoError(store.DeleteVectors(ctx, 999))
}

func TestStoreDeleteVectorsStopsBeforeMutationWhenChunkScanFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	rowsErrDeleteCount.Store(0)

	db, err := sql.Open(rowsErrDriverName, "")
	require.NoError(err)
	t.Cleanup(func() { require.NoError(db.Close()) })

	store, err := sqlitevec.New[int64, int64](ctx, db, sqlitevec.Schema{
		DocsTable:      "messages",
		IDColumn:       "id",
		ContentColumn:  "body",
		EmbedGenColumn: "embed_gen",
		VectorsPrefix:  "message_vectors",
	})
	require.NoError(err)

	err = store.DeleteVectors(ctx, 1)
	require.ErrorIs(err, io.ErrUnexpectedEOF)
	assert.Equal(int64(0), rowsErrDeleteCount.Load(), "no vector or chunk rows are deleted after a partial chunk-map scan")
}

func TestNewRejectsUnsafeIdentifiers(t *testing.T) {
	_, err := sqlitevec.New[int64, int64](t.Context(), nil, sqlitevec.Schema{
		DocsTable: "messages; DROP TABLE messages",
	})
	require.Error(t, err)
}
