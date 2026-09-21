package sqlitefts_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/sqlitefts"
	_ "modernc.org/sqlite"
)

func TestBuildAndCompose(t *testing.T) {
	q, err := sqlitefts.Build(sqlitefts.Request{
		Mapping:         sqlitefts.Mapping{IndexTable: "docs_fts", IndexKey: "source_id", SourceTable: "docs", SourceKey: "id"},
		Match:           "alpha",
		SourcePredicate: sqlitefts.Predicate{SQL: "d.tenant_id = ?", Args: []any{"tenant"}},
		ExtraSourceCols: []sqlitefts.Column{{Name: "title", As: "title"}}, Limit: 4,
	})
	require.NoError(t, err)

	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `CREATE TABLE docs (id TEXT PRIMARY KEY, tenant_id TEXT, title TEXT);
CREATE VIRTUAL TABLE docs_fts USING fts5(source_id, body);
INSERT INTO docs VALUES ('a','tenant','Alpha one'), ('b','other','Alpha two'), ('c','tenant','Alpha three');
INSERT INTO docs_fts VALUES ('a','alpha one'), ('b','alpha two'), ('c','alpha three');`)
	require.NoError(t, err)

	// The generated SELECT can be embedded as a candidate relation.
	rows, err := db.QueryContext(t.Context(), "SELECT doc_key, title, score FROM ("+q.SQL+") candidates ORDER BY score DESC, doc_key", q.Args...)
	require.NoError(t, err)
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id, title string
		var score float64
		require.NoError(t, rows.Scan(&id, &title, &score))
		require.Greater(t, score, 0.0)
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{"a", "c"}, ids)
}

func TestRowIDMappingFiltersBeforeLimitAndScansTransaction(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `CREATE TABLE docs (id INTEGER PRIMARY KEY, tenant TEXT, title TEXT);
CREATE VIRTUAL TABLE docs_fts USING fts5(body);
INSERT INTO docs VALUES (1,'other','excluded'), (2,'tenant','kept');
INSERT INTO docs_fts(rowid,body) VALUES (1,'alpha alpha'), (2,'alpha');`)
	require.NoError(t, err)
	q, err := sqlitefts.Build(sqlitefts.Request{
		Mapping: sqlitefts.Mapping{IndexTable: "docs_fts", IndexKey: "rowid", SourceTable: "docs", SourceKey: "id"},
		Match:   "alpha", SourcePredicate: sqlitefts.Predicate{SQL: "d.tenant = ?", Args: []any{"tenant"}}, Limit: 1,
	})
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	values, err := q.All(t.Context(), tx, func(rows *sql.Rows) (int, error) {
		var id int
		var score float64
		err := rows.Scan(&id, &score)
		return id, err
	})
	require.NoError(t, tx.Rollback())
	require.NoError(t, err)
	require.Equal(t, []int{2}, values)
}

func TestBuildRejectsCaseInsensitiveFixedAliasCollision(t *testing.T) {
	_, err := sqlitefts.Build(sqlitefts.Request{
		Mapping: sqlitefts.Mapping{IndexTable: "fts", IndexKey: "rowid", SourceTable: "docs", SourceKey: "id"},
		Match:   "x", ExtraSourceCols: []sqlitefts.Column{{Name: "title", As: "DOC_KEY"}}, Limit: 1,
	})
	require.Error(t, err)
}

func TestBuildRejectsPredicateArgsWithoutSQL(t *testing.T) {
	_, err := sqlitefts.Build(sqlitefts.Request{
		Mapping: sqlitefts.Mapping{IndexTable: "fts", IndexKey: "source_id", SourceTable: "docs", SourceKey: "id"},
		Match:   "x", SourcePredicate: sqlitefts.Predicate{Args: []any{"tenant"}}, Limit: 1,
	})
	require.Error(t, err)
}
