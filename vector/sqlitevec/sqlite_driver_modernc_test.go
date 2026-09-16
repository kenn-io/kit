//go:build windows || !cgo

package sqlitevec_test

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"go.kenn.io/kit/vector/sqlitevec"
)

func openSQLiteTestDB(tb testing.TB, dsn string) (*sql.DB, error) {
	tb.Helper()
	sqlitevec.Register()
	return sql.Open("sqlite", dsn)
}
