package embedmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"strings"
)

// LexicalAnalyzer identifies a lexical index. It is not a vector-space
// control and it is not an embedding input recipe. Changing it requires a
// lexical rebuild even when the embedding inputs stay the same.
type LexicalAnalyzer struct {
	Name               string
	Revision           string
	Normalization      string
	DictionaryRevision string
	PhraseRules        string
}

func (a LexicalAnalyzer) zero() bool {
	return a == (LexicalAnalyzer{})
}

// Validate allows an unset analyzer. A set analyzer needs a name.
func (a LexicalAnalyzer) Validate() error {
	if a.zero() {
		return nil
	}
	if strings.TrimSpace(a.Name) == "" {
		return errors.New("embed lexical analyzer name is required")
	}
	return nil
}

// Identity returns a stable id for a set analyzer. An unset analyzer returns
// an empty id.
func (a LexicalAnalyzer) Identity() (string, error) {
	if err := a.Validate(); err != nil {
		return "", err
	}
	if a.zero() {
		return "", nil
	}
	fields := map[string]string{"name": strings.TrimSpace(a.Name)}
	put(fields, "revision", a.Revision)
	put(fields, "normalization", a.Normalization)
	put(fields, "dictionary", a.DictionaryRevision)
	put(fields, "phrase_rules", a.PhraseRules)
	keys := slices.Sorted(maps.Keys(fields))
	var b strings.Builder
	b.WriteString("lexical-v1\n")
	for _, key := range keys {
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(fields[key], `\`, `\\`), "\n", `\n`))
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}

func put(fields map[string]string, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	fields[key] = value
}
