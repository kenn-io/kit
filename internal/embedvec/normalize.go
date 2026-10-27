// Package embedvec contains embedding vector arithmetic shared by the client and server.
package embedvec

import "math"

// NormalizeL2 scales a nonzero vector in place to unit length.
func NormalizeL2(vector []float32) {
	var squared float64
	for _, v := range vector {
		squared += float64(v) * float64(v)
	}
	if squared > 0 {
		scale := math.Sqrt(squared)
		for i := range vector {
			vector[i] = float32(float64(vector[i]) / scale)
		}
	}
}
