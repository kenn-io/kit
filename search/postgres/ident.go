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
// The fragment is trusted SQL; a ? inside a literal is still treated as a
// placeholder. n is the last used index and the result is the new last index.
func rebase(fragment string, n int) (string, int) {
	var b strings.Builder
	for _, r := range fragment {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteRune(r)
	}
	return b.String(), n
}

func checkIdentifier(kind, value string) error {
	if !validIdentifier(value) {
		return fmt.Errorf("postgres: invalid %s %q", kind, value)
	}
	return nil
}
