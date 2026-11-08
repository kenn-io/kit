package lexical

import (
	"errors"
	"slices"
	"strings"
	"unicode"
)

// Term holds one word, its regular spellings, and its analyzer match expression.
type Term struct {
	Text  string
	Forms []string
	Match string
}

// Natural holds optional ranking words and required phrases for an analyzer.
// With no phrases, any word can match.
type Natural struct {
	Match    string
	Words    []Term
	Phrases  []string
	Identity Identity
}

// PrepareNatural quotes words, expands regular English forms, and requires
// quoted phrases. Callers count matches with each Word's Match expression.
func (a Analyzer) PrepareNatural(text string) (Natural, error) {
	if err := a.valid(); err != nil {
		return Natural{}, err
	}
	if strings.ContainsRune(text, 0) {
		return Natural{}, errors.New("lexical: query text contains a NUL byte")
	}
	segmentHan := a.Identity.Kind == KindChinese && !containsKanaOrHangul(text)
	result := Natural{Identity: a.Identity}
	var words []string
	segments := strings.Split(text, `"`)
	for i, segment := range segments {
		if i%2 == 0 || i == len(segments)-1 {
			words = append(words, strings.Fields(segment)...)
		} else if phrase := strings.Join(strings.Fields(segment), " "); phrase != "" {
			result.Phrases = append(result.Phrases, phrase)
		}
	}
	var normalized []string
	for _, word := range words {
		if word = strings.TrimFunc(strings.ToLower(word), unicode.IsPunct); word != "" {
			normalized = append(normalized, word)
		}
	}
	hasContent := len(result.Phrases) > 0 || slices.ContainsFunc(normalized, func(word string) bool { return !IsStopWord(word) })
	for _, word := range normalized {
		if hasContent && IsStopWord(word) {
			continue
		}
		if i := slices.IndexFunc(result.Words, func(term Term) bool { return slices.Contains(term.Forms, word) }); i >= 0 {
			result.Words[i].Forms = slices.Compact(slices.Sorted(slices.Values(append(result.Words[i].Forms, inflections(word)...))))
			continue
		}
		result.Words = append(result.Words, Term{Text: word, Forms: inflections(word)})
	}
	var parts []string
	join := " OR "
	for i, term := range result.Words {
		var forms []string
		for _, form := range term.Forms {
			forms = append(forms, a.renderTerm(form))
		}
		term.Match = strings.Join(forms, " OR ")
		if len(forms) > 1 {
			term.Match = "(" + term.Match + ")"
		}
		if segmentHan && containsHan(term.Text) {
			prepared, err := a.segmented(term.Text)
			if err != nil {
				return Natural{}, err
			}
			term.Match = "(" + prepared.Match + ")"
		}
		result.Words[i] = term
		parts = append(parts, term.Match)
	}
	if len(result.Phrases) > 0 {
		parts, join = nil, " AND "
		for _, phrase := range result.Phrases {
			parts = append(parts, a.renderTerm(phrase))
		}
	}
	result.Match = strings.Join(parts, join)
	if result.Match == "" {
		return Natural{}, errors.New("lexical: query text is empty")
	}
	return result, nil
}

func inflections(word string) []string {
	forms := []string{word}
	if len(word) < 2 || IsStopWord(word) || strings.ContainsFunc(word, func(r rune) bool { return r < 'a' || r > 'z' }) {
		return forms
	}
	bases := []string{word}
	for _, suffix := range []string{"s", "es", "ies", "ed", "ied", "ing"} {
		stem, ok := strings.CutSuffix(word, suffix)
		if !ok {
			continue
		}
		bases = append(bases, stem, stem+"e", stem+"y")
		if prefix, ok := strings.CutSuffix(stem, "y"); ok {
			bases = append(bases, prefix+"ie")
		}
		if strings.HasSuffix(stem, "ck") {
			bases = append(bases, strings.TrimSuffix(stem, "k"))
		}
		if len(stem) > 1 && stem[len(stem)-1] == stem[len(stem)-2] {
			bases = append(bases, stem[:len(stem)-1])
		}
	}
	for _, base := range bases {
		if len(base) < 2 || !strings.ContainsAny(strings.TrimSuffix(base, "e"), "aeiouy") {
			continue
		}
		variants := regular(base)
		if !slices.Contains(variants, word) {
			continue
		}
		for _, form := range variants {
			if !IsStopWord(form) && !slices.Contains(forms, form) {
				forms = append(forms, form)
			}
		}
	}
	return forms
}

// regular includes its base so suffix recovery uses the same spelling rules.
func regular(base string) []string {
	plural, past, present := base+"s", base+"ed", base+"ing"
	if strings.HasSuffix(base, "s") || strings.HasSuffix(base, "x") || strings.HasSuffix(base, "z") || strings.HasSuffix(base, "ch") || strings.HasSuffix(base, "sh") {
		plural = base + "es"
	}
	var extra []string
	if strings.HasSuffix(base, "c") {
		extra = append(extra, past, present, base+"ked", base+"king")
	}
	switch {
	case strings.HasSuffix(base, "y") && !vowel(base[len(base)-2]):
		plural, past = base[:len(base)-1]+"ies", base[:len(base)-1]+"ied"
	case strings.HasSuffix(base, "ie"):
		past, present = base+"d", base[:len(base)-2]+"ying"
	case strings.HasSuffix(base, "ee") || strings.HasSuffix(base, "ye") || strings.HasSuffix(base, "oe"):
		past = base + "d"
	case strings.HasSuffix(base, "e"):
		past, present = base+"d", base[:len(base)-1]+"ing"
	default:
		sounds := strings.ReplaceAll(base, "qu", "q")
		n := len(sounds)
		if n >= 3 && !vowel(sounds[n-3]) && vowel(sounds[n-2]) && !vowel(sounds[n-1]) && !strings.ContainsRune("wxy", rune(sounds[n-1])) {
			doubled := base + base[len(base)-1:]
			groups := 0
			for i := range len(sounds) {
				if vowel(sounds[i]) && (i == 0 || !vowel(sounds[i-1])) {
					groups++
				}
			}
			if groups == 1 && strings.HasSuffix(base, "z") {
				plural = doubled + "es"
			}
			if groups > 1 {
				extra = append(extra, past, present)
			}
			past, present = doubled+"ed", doubled+"ing"
		}
	}
	if strings.HasSuffix(base, "o") {
		extra = append(extra, base+"es")
	}
	return append([]string{base, plural, past, present}, extra...)
}

func vowel(c byte) bool { return strings.ContainsRune("aeiouy", rune(c)) }
