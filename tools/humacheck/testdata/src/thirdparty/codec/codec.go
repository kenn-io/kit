// Package codec stands in for an external JSON library the checker cannot
// see into.
package codec

import "io"

func Marshal(w io.Writer, v any) error   { return nil }
func Unmarshal(data []byte, v any) error { return nil }
