// Package vectorliteral formats float32 vectors for SQL vector extensions.
package vectorliteral

import (
	"strconv"
	"strings"
)

// Format returns the bracketed representation accepted by vector extensions.
func Format(vector []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, value := range vector {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(value), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
