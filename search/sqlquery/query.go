// Package sqlquery executes parameterized search relations through a caller's
// database handle. Backend packages construct the SQL and define its columns.
package sqlquery

import (
	"context"
	"database/sql"
	"fmt"
)

// Query is a composable SELECT and its bound arguments. The producing backend
// defines the SQL dialect, placeholders and output columns. When composing it,
// callers must preserve argument order and rebase numbered placeholders where
// required by that dialect. A composed outer query must specify its own order.
type Query struct {
	SQL  string
	Args []any
}

// Queryer is the query capability shared by *sql.DB, *sql.Tx and *sql.Conn.
// The caller retains ownership of the handle and any transaction.
type Queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// All executes a search relation and maps its rows to caller-defined values.
// scan reads the current row only; All owns iteration and closing the rows.
// On any query, scan or iteration error, All returns no partial result.
func (q Query) All[T any](ctx context.Context, db Queryer, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := db.QueryContext(ctx, q.SQL, q.Args...)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []T
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan search row: %w", err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read search rows: %w", err)
	}
	return result, nil
}
