//go:build !windows && cgo

package sqlitevec

import vecext "github.com/asg017/sqlite-vec-go-bindings/cgo"

// Register loads the sqlite-vec extension into every SQLite connection
// opened afterwards in this process. It must be called before opening the
// database the store will use.
func Register() { vecext.Auto() }

func vectorValue(vector []float32) (string, any, error) {
	blob, err := vecext.SerializeFloat32(vector)
	if err != nil {
		return "", nil, err
	}
	return "?", blob, nil
}
