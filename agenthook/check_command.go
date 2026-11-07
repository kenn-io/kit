package agenthook

import (
	"errors"
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/pattern"
	"mvdan.cc/sh/v3/syntax"
)

func posixHookExecutable(command string) (string, error) {
	parsed, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return "", fmt.Errorf("parse hook command: %w", err)
	}
	if len(parsed.Stmts) != 1 {
		return "", errors.New("hook command requires shell interpretation")
	}
	stmt := parsed.Stmts[0]
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) > 0 || len(call.Args) == 0 || stmt.Background || stmt.Coprocess || stmt.Disown || stmt.Negated || len(stmt.Redirs) > 0 {
		return "", errors.New("hook command requires shell interpretation")
	}
	literal := true
	word := call.Args[0]
	syntax.SplitBraces(word)
	syntax.Walk(word, func(node syntax.Node) bool {
		switch node.(type) {
		case *syntax.Word, *syntax.Lit, *syntax.SglQuoted, *syntax.DblQuoted, nil:
			return true
		default:
			literal = false
			return false
		}
	})
	for i, part := range word.Parts {
		if text, ok := part.(*syntax.Lit); ok && (pattern.HasMeta(text.Value, 0) || (i == 0 && strings.HasPrefix(text.Value, "~"))) {
			literal = false
		}
	}
	if !literal {
		return "", errors.New("hook command requires shell interpretation")
	}
	words, err := expand.Fields(nil, word)
	if err != nil || len(words) == 0 {
		return "", fmt.Errorf("cannot identify hook executable in %q", command)
	}
	return words[0], nil
}
