package transfer

import (
	"context"
	"database/sql"
	"sync"
)

func Query(ctx context.Context, db *sql.DB, mu *sync.Mutex) (*sql.Rows, error) {
	mu.Lock()
	defer mu.Unlock()
	rows, err := db.QueryContext(ctx, "SELECT value FROM items")
	return rows, err
}
func Named(ctx context.Context, db *sql.DB, mu *sync.Mutex) (rows *sql.Rows, err error) {
	mu.Lock()
	defer mu.Unlock()
	rows, err = db.QueryContext(ctx, "SELECT value FROM items")
	return
}

type Row struct {
	rows *sql.Rows
	err  error
}

func (r Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer r.rows.Close()
	if !r.rows.Next() {
		return r.rows.Err()
	}
	return r.rows.Scan(dest...)
}
func Single(ctx context.Context, db *sql.DB) interface{ Scan(...any) error } {
	rows, err := db.QueryContext(ctx, "SELECT value FROM items")
	return Row{rows, err}
}
