package clisearch

import (
	"slices"
	"strings"
	"unicode"
)

// tokens keeps repetition for field term frequency. Query terms and expansion
// candidates are deduplicated separately. This deliberately small English fold
// also preserves Unicode words; punctuation never becomes search syntax.
func tokens(text string) []string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(words))
	for _, word := range words {
		switch word {
		case "a", "an", "the", "and", "or", "of", "to", "for", "from", "in", "on", "at", "by", "with", "as", "is", "are", "be", "i", "me", "my", "it", "this", "that", "how", "do", "does", "can", "please":
			continue
		}
		out = append(out, fold(word))
	}
	return out
}

func fold(word string) string {
	switch {
	case len(word) > 4 && strings.HasSuffix(word, "ies"):
		word = strings.TrimSuffix(word, "ies") + "y"
	case len(word) > 4 && (strings.HasSuffix(word, "ches") || strings.HasSuffix(word, "shes") || strings.HasSuffix(word, "xes") || strings.HasSuffix(word, "zes") || strings.HasSuffix(word, "sses") || strings.HasSuffix(word, "oes")):
		word = word[:len(word)-2]
	case len(word) > 3 && strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss") && !strings.HasSuffix(word, "us") && !strings.HasSuffix(word, "is"):
		word = word[:len(word)-1]
	}
	inflected := false
	switch {
	case len(word) > 5 && strings.HasSuffix(word, "ing"):
		word = word[:len(word)-3]
		inflected = true
	case len(word) > 4 && strings.HasSuffix(word, "ed"):
		word = word[:len(word)-2]
		inflected = true
	}
	// A three-letter stem can already contain an intrinsic doubled consonant, as in "add".
	if inflected && len(word) > 3 && word[len(word)-1] == word[len(word)-2] && strings.ContainsRune("bdgmnprt", rune(word[len(word)-1])) {
		word = word[:len(word)-1]
	}
	if len(word) >= 4 {
		word = strings.TrimSuffix(word, "e")
	}
	return word
}

var genericSynonyms = func() map[string][]string {
	result := make(map[string][]string)
	for _, group := range []string{"remove delete rm", "list show ls", "create add new", "todo task", "find search lookup"} {
		words := tokens(group)
		for _, word := range words {
			result[word] = words
		}
	}
	return result
}()

type candidate struct {
	text   string
	weight float64
}

func expand(query string, synonyms map[string][]string) [][]candidate {
	words := tokens(query)
	slices.Sort(words)
	words = slices.Compact(words)
	extra := make(map[string][]string)
	// Normalization can collapse multiple keys. Appending all candidates and
	// sorting below makes that merge independent of map iteration order.
	for key, values := range synonyms {
		for _, word := range tokens(key) {
			for _, value := range values {
				extra[word] = append(extra[word], tokens(value)...)
			}
		}
	}
	groups := make([][]candidate, 0, len(words))
	for _, word := range words {
		alternatives := append(slices.Clone(genericSynonyms[word]), extra[word]...)
		alternatives = append(alternatives, word)
		slices.Sort(alternatives)
		alternatives = slices.Compact(alternatives)
		group := make([]candidate, 0, len(alternatives))
		for _, alternative := range alternatives {
			weight := 0.6
			if alternative == word {
				weight = 1
			}
			group = append(group, candidate{alternative, weight})
		}
		groups = append(groups, group)
	}
	return groups
}
