package vector

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// Generation identifies an embedding model configuration. Two pieces of
// content embedded under generations with the same Fingerprint share a
// vector space; a different fingerprint means the caller should treat the
// vectors as a new generation and re-embed.
type Generation struct {
	// Model names the embedding model, e.g. "text-embedding-3-small".
	Model string
	// Dimensions is the length of the vectors the model emits.
	Dimensions int
	// Params holds any additional knobs that change the vector space,
	// such as a pooling mode or prompt template. Keys are sorted before
	// fingerprinting, so map iteration order never affects the result.
	Params map[string]string
}

// Fingerprint returns a stable identifier derived from every field that
// affects the vector space. Callers persist it alongside stored vectors
// and compare it to decide whether a new generation is required.
func (g Generation) Fingerprint() string {
	var b strings.Builder
	b.WriteString(g.Model)
	b.WriteByte('\n')
	b.WriteString(strconv.Itoa(g.Dimensions))
	b.WriteByte('\n')

	keys := make([]string, 0, len(g.Params))
	for k := range g.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(g.Params[k])
		b.WriteByte('\n')
	}

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}
