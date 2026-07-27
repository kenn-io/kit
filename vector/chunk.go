package vector

import "strings"

// Chunk is a window of text encoded as a single vector. Index is the
// chunk's position within the source content, starting at zero.
type Chunk struct {
	Index int
	Text  string
}

// SplitOptions controls how Split windows content into chunks.
type SplitOptions struct {
	// MaxRunes bounds the number of runes in each chunk. Values <= 0
	// disable splitting and return the content as a single chunk.
	MaxRunes int
	// Overlap is the number of runes shared between consecutive chunks.
	// It is clamped to the range [0, MaxRunes-1].
	Overlap int
}

// Split windows content into overlapping chunks of at most MaxRunes runes.
// It splits on runes rather than bytes so multi-byte characters are never
// torn apart. Empty or Unicode-whitespace-only windows yield no chunks, so
// callers never need to send semantically empty text to an embedding model.
//
// Split measures size in runes, not model tokens. Callers that budget by
// tokens should convert their token budget to an approximate rune count.
func Split(content string, o SplitOptions) []Chunk {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	runes := []rune(content)
	if o.MaxRunes <= 0 || len(runes) <= o.MaxRunes {
		return []Chunk{{Index: 0, Text: content}}
	}

	overlap := min(max(o.Overlap, 0), o.MaxRunes-1)
	stride := o.MaxRunes - overlap

	var chunks []Chunk
	for start, idx := 0, 0; start < len(runes); start, idx = start+stride, idx+1 {
		end := start + o.MaxRunes
		if end >= len(runes) {
			text := string(runes[start:])
			if strings.TrimSpace(text) != "" {
				chunks = append(chunks, Chunk{Index: idx, Text: text})
			}
			break
		}
		text := string(runes[start:end])
		if strings.TrimSpace(text) != "" {
			chunks = append(chunks, Chunk{Index: idx, Text: text})
		}
	}
	return chunks
}
