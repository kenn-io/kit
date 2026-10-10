package app

import (
	"context"
	"database/sql"

	"dep"
)

func Count(ctx context.Context, db *sql.DB) (int, error) {
	rows, err := db.QueryContext(ctx, "SELECT 1")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if err := dep.Wait(ctx); err != nil {
		return 0, err
	}
	return dep.Count(rows)
}
