package dep

import (
	"context"
	"database/sql"
)

// Count is a scanner: callers that pass it rows have their error checked.
func Count(rows *sql.Rows) (int, error) {
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

// Wait checks Err, but on a context rather than rows.
func Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
