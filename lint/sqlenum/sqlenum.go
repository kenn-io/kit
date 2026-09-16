// Package sqlenum reports CHECK constraints that hard-code the allowed values
// of a column, such as CHECK (status IN ('queued', 'done')).
//
// Such constraints look like validation but behave like a schema lock: every
// new value needs a migration that drops and recreates the constraint (and on
// SQLite, often the whole table). The set of values belongs in application
// code or a lookup table, where it can change without a schema migration.
//
// The package scans raw SQL (migration files) and, through Analyzer, SQL held
// in Go string literals.
package sqlenum

import (
	"fmt"
	"go/ast"
	"go/token"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// Finding is one enum-style CHECK constraint in a SQL source.
type Finding struct {
	// Offset is the byte offset of the CHECK keyword in the source.
	Offset int
	// Line and Column are 1-based positions of the CHECK keyword.
	Line, Column int
	// Column name whose values the constraint enumerates.
	ColumnName string
	// Expr is the constraint expression as written.
	Expr string
}

// Message renders the diagnostic for a finding.
func (f Finding) Message() string {
	return fmt.Sprintf("CHECK constraint hard-codes the allowed values of %s; every new value then needs a schema migration to rewrite the constraint. Validate the set in application code or keep it in a lookup table", f.ColumnName)
}

var (
	checkKeyword = regexp.MustCompile(`(?i)\bCHECK\s*\(`)
	whitespace   = regexp.MustCompile(`\s+`)

	// ident is a possibly quoted or qualified column reference, optionally
	// wrapped in a case-folding function.
	ident = `(?:(?:lower|upper)\s*\(\s*)?(?:"[^"]+"|` + "`[^`]+`" + `|\[[^\]]+\]|[\w.]+)(?:\s*\))?`
	// literal is a SQL string or integer constant.
	literal = `(?:'(?:[^']|'')*'|\d+)`

	inList  = regexp.MustCompile(`(?i)^(` + ident + `)\s+(?:not\s+)?in\s*\(\s*` + literal + `(?:\s*,\s*` + literal + `)*\s*\)$`)
	orChain = regexp.MustCompile(`(?i)^(` + ident + `)\s*(?:=|<>|!=)\s*` + literal + `(?:\s+(?:or|and)\s+` + ident + `\s*(?:=|<>|!=)\s*` + literal + `)+$`)
)

// Scan returns every enum-style CHECK constraint in src.
func Scan(src string) []Finding {
	var findings []Finding
	for _, loc := range checkKeyword.FindAllStringIndex(src, -1) {
		open := loc[1] - 1
		end := matchParen(src, open)
		if end < 0 {
			continue
		}
		expr := strings.TrimSpace(src[open+1 : end])
		column, ok := enumeratedColumn(expr)
		if !ok {
			continue
		}
		line, col := position(src, loc[0])
		findings = append(findings, Finding{Offset: loc[0], Line: line, Column: col, ColumnName: column, Expr: expr})
	}
	return findings
}

func enumeratedColumn(expr string) (string, bool) {
	normalized := whitespace.ReplaceAllString(expr, " ")
	for _, re := range []*regexp.Regexp{inList, orChain} {
		if m := re.FindStringSubmatch(normalized); m != nil {
			name := m[1]
			if i := strings.IndexByte(name, '('); i >= 0 {
				name = strings.TrimSuffix(strings.TrimSpace(name[i+1:]), ")")
			}
			return strings.TrimSpace(name), true
		}
	}
	return "", false
}

// matchParen returns the index of the parenthesis closing the one at open,
// skipping string literals, or -1.
func matchParen(src string, open int) int {
	depth := 0
	inString := false
	for i := open; i < len(src); i++ {
		c := src[i]
		switch {
		case inString:
			if c == '\'' {
				inString = false
			}
		case c == '\'':
			inString = true
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func position(src string, offset int) (line, column int) {
	line = 1 + strings.Count(src[:offset], "\n")
	column = offset - strings.LastIndexByte(src[:offset], '\n')
	return line, column
}

// Analyzer reports enum-style CHECK constraints inside Go string literals in
// non-test files, where embedded schema and migration SQL usually lives.
var Analyzer = &analysis.Analyzer{
	Name:     "sqlenum",
	Doc:      "reports SQL CHECK constraints that hard-code a column's allowed values",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	inspect.Preorder([]ast.Node{(*ast.BasicLit)(nil)}, func(n ast.Node) {
		lit := n.(*ast.BasicLit)
		if lit.Kind != token.STRING || strings.HasSuffix(pass.Fset.Position(lit.Pos()).Filename, "_test.go") {
			return
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			return
		}
		for _, f := range Scan(value) {
			pos := lit.Pos()
			if lit.Value[0] == '`' {
				// Raw strings map byte offsets directly; escapes in
				// interpreted strings do not, so those report at the literal.
				pos += token.Pos(1 + f.Offset)
			}
			pass.Reportf(pos, "%s", f.Message())
		}
	})
	return nil, nil
}
