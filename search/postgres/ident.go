package postgres

import (
	"fmt"
	"strings"
)

func validIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// rebase replaces anonymous ? placeholders with PostgreSQL $n placeholders.
// A ? inside a string, dollar quote, quoted identifier, or comment stays
// literal. ?? outside those regions is one literal ? and does not consume a
// placeholder number. n is the last used index and the result is the new one.
func rebase(fragment string, n int) (string, int) {
	var b strings.Builder
	b.Grow(len(fragment))
	for i := 0; i < len(fragment); {
		if end, ok := literalEnd(fragment, i); ok {
			b.WriteString(fragment[i:end])
			i = end
			continue
		}
		if fragment[i] == '?' {
			if i+1 < len(fragment) && fragment[i+1] == '?' {
				b.WriteByte('?')
				i += 2
				continue
			}
			n++
			fmt.Fprintf(&b, "$%d", n)
			i++
			continue
		}
		b.WriteByte(fragment[i])
		i++
	}
	return b.String(), n
}

// literalEnd reports the exclusive end of a quote, dollar quote, or comment
// that starts at i.
func literalEnd(s string, i int) (int, bool) {
	switch {
	case hasPrefixAt(s, i, "--"):
		return lineCommentEnd(s, i), true
	case hasPrefixAt(s, i, "/*"):
		return blockCommentEnd(s, i), true
	case s[i] == '\'', s[i] == '"':
		return quotedEnd(s, i, s[i]), true
	case s[i] == '$':
		if end, ok := dollarQuoteEnd(s, i); ok {
			return end, true
		}
	}
	return 0, false
}

func hasPrefixAt(s string, i int, prefix string) bool {
	return i+len(prefix) <= len(s) && s[i:i+len(prefix)] == prefix
}

func lineCommentEnd(s string, i int) int {
	if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
		return i + j + 1
	}
	return len(s)
}

func blockCommentEnd(s string, i int) int {
	if j := strings.Index(s[i+2:], "*/"); j >= 0 {
		return i + 2 + j + 2
	}
	return len(s)
}

func quotedEnd(s string, i int, quote byte) int {
	j := i + 1
	for j < len(s) {
		if s[j] != quote {
			j++
			continue
		}
		if j+1 < len(s) && s[j+1] == quote {
			j += 2
			continue
		}
		return j + 1
	}
	return len(s)
}

// dollarQuoteEnd recognizes $tag$ ... $tag$ (the tag may be empty). Tags use
// the same ASCII identifier characters as validIdentifier.
func dollarQuoteEnd(s string, i int) (int, bool) {
	if i >= len(s) || s[i] != '$' {
		return 0, false
	}
	j := i + 1
	for j < len(s) && s[j] != '$' {
		if !dollarTagByte(s[j], j == i+1) {
			return 0, false
		}
		j++
	}
	if j >= len(s) || s[j] != '$' {
		return 0, false
	}
	delim := s[i : j+1]
	k := strings.Index(s[j+1:], delim)
	if k < 0 {
		return len(s), true
	}
	return j + 1 + k + len(delim), true
}

func dollarTagByte(c byte, first bool) bool {
	letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
	if first {
		return letter
	}
	return letter || (c >= '0' && c <= '9')
}

func checkIdentifier(kind, value string) error {
	if !validIdentifier(value) {
		return fmt.Errorf("postgres: invalid %s %q", kind, value)
	}
	return nil
}
