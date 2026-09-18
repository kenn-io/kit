package scanner

import "database/sql"

type Rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func Read(rows Rows) error {
	for rows.Next() {
		var value int
		if err := rows.Scan(&value); err != nil {
			return err
		}
	}
	return rows.Err()
}
func Forward(rows *sql.Rows) error { return Read(rows) }
func Values(rows Rows) ([]int, error) {
	var values []int
	for rows.Next() {
		var value int
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
func Unchecked(rows Rows) error {
	for rows.Next() {
		var value int
		if err := rows.Scan(&value); err != nil {
			return err
		}
	}
	return nil
}
