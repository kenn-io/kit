package leaks

import (
	"context"
	"database/sql"
	"sync"
)

func Leak(ctx context.Context, db *sql.DB, mu *sync.Mutex) error {
	mu.Lock()
	defer mu.Unlock()
	rows, err := db.QueryContext(ctx, "SELECT value FROM items") // want "Rows/Stmt/NamedStmt was not closed"
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	return rows.Err()
}
func ReturnOther(ctx context.Context, db *sql.DB, other *sql.Rows, mu *sync.Mutex) (*sql.Rows, error) {
	mu.Lock()
	defer mu.Unlock()
	rows, err := db.QueryContext(ctx, "SELECT value FROM items") // want "Rows/Stmt/NamedStmt was not closed"
	if err != nil {
		return nil, err
	}
	rows.Next()
	return other, nil
}
