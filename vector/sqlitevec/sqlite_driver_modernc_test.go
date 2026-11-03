//go:build windows || !cgo

package sqlitevec_test

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	_ "modernc.org/sqlite/vec"
)

func openSQLiteTestDB(tb testing.TB, dsn string) (*sql.DB, error) {
	tb.Helper()
	// Disposable test databases don't need crash durability.
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	dsn += separator + "_pragma=synchronous(OFF)"
	return sql.Open("sqlite", dsn)
}
