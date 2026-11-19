// Package vectorliteral formats vectors for storage APIs that accept text literals.
package vectorliteral

import (
	"strconv"
	"strings"
)

// Format returns a bracketed, comma-separated float32 literal.
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
