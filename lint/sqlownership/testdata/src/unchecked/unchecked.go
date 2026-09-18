package unchecked

import (
	"context"
	"database/sql"
	"scanner"
)

func NoCheck(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT value FROM items") // want "rows.Err must be checked"
	if err != nil {
		return err
	}
	defer rows.Close()
	return scanner.Unchecked(rows)
}
