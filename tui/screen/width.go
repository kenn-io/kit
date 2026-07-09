package screen

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Width returns the display width of the widest line in s. ANSI control
// sequences have zero width and Unicode grapheme clusters occupy the number of
// terminal cells reported by ansi.StringWidth.
func Width(s string) int {
	widest := 0
	for line := range strings.SplitSeq(s, "\n") {
		widest = max(widest, ansi.StringWidth(line))
	}
	return widest
}

// Prefix returns the printable prefix of line that fits in maxWidth terminal
// cells and the width actually occupied. It never splits an ANSI sequence or
// Unicode grapheme. Zero-width control sequences needed to close terminal
// state after the retained text are preserved. A non-positive maxWidth returns
// an empty prefix.
func Prefix(line string, maxWidth int) (prefix string, width int) {
	if line == "" || maxWidth <= 0 {
		return "", 0
	}
	prefix = ansi.Truncate(line, maxWidth, "")
	return prefix, ansi.StringWidth(prefix)
}

// Truncate returns the printable prefix of line that fits in maxWidth terminal
// cells. It is the string-only form of Prefix.
func Truncate(line string, maxWidth int) string {
	prefix, _ := Prefix(line, maxWidth)
	return prefix
}

// Suffix returns line after skipping at least skipWidth terminal cells. ANSI
// sequences encountered before the boundary are replayed before the suffix so
// styles and OSC state active at the boundary remain active. If the boundary
// crosses a wide grapheme, the complete grapheme is skipped. A non-positive
// skipWidth returns line unchanged.
func Suffix(line string, skipWidth int) string {
	suffix, _ := suffixWithWidth(line, skipWidth)
	return suffix
}

func suffixWithWidth(line string, skipWidth int) (suffix string, skippedWidth int) {
	if line == "" || skipWidth <= 0 {
		return line, 0
	}

	var carried strings.Builder
	state := ansi.NormalState
	used := 0
	offset := 0
	for offset < len(line) && used < skipWidth {
		sequence, width, bytesRead, nextState := ansi.DecodeSequence(line[offset:], state, nil)
		if bytesRead == 0 {
			// Invalid UTF-8 cannot advance the decoder. Preserve the remaining
			// input rather than looping or silently dropping bytes.
			return carried.String() + line[offset:], used
		}
		if width == 0 {
			carried.WriteString(sequence)
		}
		used += width
		offset += bytesRead
		state = nextState
	}
	if offset >= len(line) {
		return "", used
	}
	return carried.String() + line[offset:], used
}
