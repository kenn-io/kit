package sqlitevec

import (
	"database/sql"
	"testing"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// TestVec0LoadsHermetically confirms the sqlite-vec extension is compiled
// in via the cgo binding, so the backend's tests need no external setup.
func TestVec0LoadsHermetically(t *testing.T) {
	require := require.New(t)
	sqlitevec.Auto()

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(err)
	t.Cleanup(func() { require.NoError(db.Close()) })

	_, err = db.Exec(`CREATE VIRTUAL TABLE v USING vec0(embedding float[3])`)
	require.NoError(err)

	vec, err := sqlitevec.SerializeFloat32([]float32{1, 2, 3})
	require.NoError(err)
	_, err = db.Exec(`INSERT INTO v(rowid, embedding) VALUES (1, ?)`, vec)
	require.NoError(err)

	query, err := sqlitevec.SerializeFloat32([]float32{1, 2, 3})
	require.NoError(err)
	var rowid int64
	var distance float64
	err = db.QueryRow(
		`SELECT rowid, distance FROM v WHERE embedding MATCH ? ORDER BY distance LIMIT 1`,
		query,
	).Scan(&rowid, &distance)
	require.NoError(err)

	require.Equal(int64(1), rowid)
	require.InDelta(0, distance, 1e-6, "identical vectors have zero distance")
}
