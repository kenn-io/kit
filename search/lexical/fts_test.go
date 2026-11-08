package lexical_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func ftsDB(t *testing.T, bodies []string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `CREATE TABLE docs(id INTEGER PRIMARY KEY, body TEXT); CREATE VIRTUAL TABLE docs_fts USING fts5(body, tokenize='unicode61');`)
	require.NoError(t, err)
	for _, body := range bodies {
		_, err = db.ExecContext(t.Context(), `INSERT INTO docs(body) VALUES (?)`, body)
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `INSERT INTO docs_fts(body) VALUES (?)`, body)
		require.NoError(t, err)
	}
	return db
}

func matchBodies(t *testing.T, db *sql.DB, match string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT d.body FROM docs_fts JOIN docs AS d ON d.id = docs_fts.rowid WHERE docs_fts MATCH ?`, match)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var hits []string
	for rows.Next() {
		var body string
		require.NoError(t, rows.Scan(&body))
		hits = append(hits, body)
	}
	require.NoError(t, rows.Err())
	return hits
}
