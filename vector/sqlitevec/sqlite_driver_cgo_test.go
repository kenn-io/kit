//go:build !windows && cgo

package sqlitevec_test

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"go.kenn.io/kit/vector/sqlitevec"
)

func openSQLiteTestDB(t testing.TB, dsn string) (*sql.DB, error) {
	t.Helper()
	sqlitevec.Register()
	return sql.Open("sqlite3", dsn)
}
