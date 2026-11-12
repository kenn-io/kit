package agenthook

import (
	"errors"
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/pattern"
	"mvdan.cc/sh/v3/syntax"
)

// posixHookWords returns a literal executable followed by each argument's
// value. An argument the shell would expand keeps its source text, prefixed
// with NUL so it never equals a literal value, and is never evaluated.
func posixHookWords(command string) ([]string, error) {
	parsed, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, fmt.Errorf("parse hook command: %w", err)
	}
	if len(parsed.Stmts) != 1 {
		return nil, errors.New("hook command requires shell interpretation")
	}
	stmt := parsed.Stmts[0]
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) > 0 || len(call.Args) == 0 || stmt.Background || stmt.Coprocess || stmt.Disown || stmt.Negated || len(stmt.Redirs) > 0 {
		return nil, errors.New("hook command requires shell interpretation")
	}
	words := make([]string, len(call.Args))
	for i, word := range call.Args {
		value, ok := posixLiteral(word)
		if !ok && i == 0 {
			return nil, errors.New("hook command requires shell interpretation")
		}
		if !ok {
			value = "\x00" + command[word.Pos().Offset():word.End().Offset()]
		}
		words[i] = value
	}
	return words, nil
}

func posixLiteral(word *syntax.Word) (string, bool) {
	literal := true
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
		return "", false
	}
	fields, err := expand.Fields(nil, word)
	if err != nil || len(fields) != 1 {
		return "", false
	}
	return fields[0], true
}
