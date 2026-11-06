//go:build !windows

package agenthook

import (
	"errors"
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/shell"
	"mvdan.cc/sh/v3/syntax"
)

func rawHookExecutable(command string) (string, error) {
	// Inspect quoting before expanding literal words. Shell operations and
	// unquoted patterns cannot establish an executable without evaluation.
	for word, err := range syntax.NewParser().WordsSeq(strings.NewReader(command)) {
		if err != nil {
			return "", fmt.Errorf("parse hook command: %w", err)
		}
		literal := word.Pos().Line() == 1
		syntax.Walk(word, func(node syntax.Node) bool {
			switch node.(type) {
			case *syntax.Word, *syntax.Lit, *syntax.SglQuoted, *syntax.DblQuoted, nil:
				return true
			default:
				literal = false
				return false
			}
		})
		for _, part := range word.Parts {
			if text, ok := part.(*syntax.Lit); ok && strings.ContainsAny(text.Value, "~*?{}[]") {
				literal = false
			}
		}
		if !literal {
			return "", errors.New("hook command requires shell interpretation")
		}
	}

	words, err := shell.Fields(command, func(string) string { return "" })
	if err != nil || len(words) == 0 {
		return "", fmt.Errorf("cannot identify hook executable in %q", command)
	}
	return words[0], nil
}
