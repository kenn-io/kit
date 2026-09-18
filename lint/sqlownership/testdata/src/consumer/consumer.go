package consumer

import (
	"context"
	"database/sql"
	"scanner"
)

func Read(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT value FROM items")
	if err != nil {
		return err
	}
	defer rows.Close()
	return scanner.Read(rows)
}
func Forward(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT value FROM items")
	if err != nil {
		return err
	}
	defer rows.Close()
	return scanner.Forward(rows)
}
