package vector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Generation identifies an embedding model configuration. Two pieces of
// content embedded under generations with the same Fingerprint share a
// vector space; a different fingerprint means the caller should treat the
// vectors as a new generation and re-embed.
type Generation struct {
	// Model names the embedding model, e.g. "text-embedding-3-small".
	Model string `json:"model,omitzero"`
	// Dimensions is the length of the vectors the model emits.
	Dimensions int `json:"dimensions,omitzero"`
	// Params holds any additional knobs that change the vector space,
	// such as a pooling mode or prompt template.
	Params map[string]string `json:"params,omitzero"`
}

// Fingerprint returns a stable identifier derived from every field that
// affects the vector space. Callers persist it alongside stored vectors
// and compare it to decide whether a new generation is required.
//
// It hashes the JSON encoding rather than a hand-built string so that
// values are escaped and delimited unambiguously: a param value that
// itself contains the separator can never collide with two distinct
// params.
func (g Generation) Fingerprint() string {
	// Generation holds only strings, an int, and a string map, all of
	// which encoding/json can always marshal — and it sorts map keys, so
	// the encoding is canonical. The error is structurally unreachable.
	data, _ := json.Marshal(g)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}
