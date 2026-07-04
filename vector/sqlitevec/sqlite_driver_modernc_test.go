//go:build windows || !cgo

package sqlitevec_test

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"go.kenn.io/kit/vector/sqlitevec"
)

func openSQLiteTestDB(t testing.TB, dsn string) (*sql.DB, error) {
	t.Helper()
	sqlitevec.Register()
	return sql.Open("sqlite", dsn)
}
