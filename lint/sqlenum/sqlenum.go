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
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// Kind classifies what a Finding reports.
type Kind int

const (
	// CheckConstraint is a CHECK (...) that compares a column against a
	// fixed list of literal values.
	CheckConstraint Kind = iota
	// EnumType is a CREATE TYPE ... AS ENUM (...) declaration.
	EnumType
)

// Finding is one enum-style schema construct in a SQL source.
type Finding struct {
	// Kind says whether the finding is a CHECK constraint or an enum type.
	Kind Kind
	// Offset is the byte offset of the CHECK or CREATE TYPE keyword.
	Offset int
	// Line and Column are 1-based positions of the keyword.
	Line, Column int
	// ColumnName is the column whose values the constraint enumerates, or
	// the type name for an enum type.
	ColumnName string
	// Expr is the constraint expression or the enum value list as written.
	Expr string
}

// Message renders the diagnostic for a finding.
func (f Finding) Message() string {
	if f.Kind == EnumType {
		return fmt.Sprintf("enum type %s hard-codes its allowed values in the schema; every new value then needs a migration to alter the type. Store the column as text and validate the set in application code or a lookup table", f.ColumnName)
	}
	return fmt.Sprintf("CHECK constraint hard-codes the allowed values of %s; every new value then needs a schema migration to rewrite the constraint. Validate the set in application code or keep it in a lookup table", f.ColumnName)
}

var (
	checkKeyword = regexp.MustCompile(`(?i)\bCHECK\s*\(`)
	enumKeyword  = regexp.MustCompile(`(?i)\bCREATE\s+TYPE\s+(` + name + `)\s+AS\s+ENUM\s*\(`)
	whitespace   = regexp.MustCompile(`\s+`)

	// name is a possibly quoted or qualified column reference; ident is a
	// name optionally wrapped in a case-folding function.
	name  = `(?:"[^"]+"|` + "`[^`]+`" + `|\[[^\]]+\]|[A-Za-z_]\w*)(?:\.(?:"[^"]+"|\w+))*`
	ident = `(?:(?:lower|upper)\s*\(\s*` + name + `\s*\)|` + name + `)`
	// literal is a SQL string or integer constant, optionally cast.
	literal = `(?:'(?:[^']|'')*'|\d+)(?:::\w+(?:\[\])?)?`
	// literals is a comma-separated list of at least one literal.
	literals = literal + `(?:\s*,\s*` + literal + `)*`

	// boundary keeps ident from matching the tail of a longer token such as
	// a function argument list or a numeric literal.
	boundary = `(?:^|[\s(])`

	// inList matches "col IN ('a', 'b')" and "col NOT IN (...)".
	inList = regexp.MustCompile(`(?i)` + boundary + `(` + ident + `)\s+(?:not\s+)?in\s*\(\s*` + literals + `\s*\)`)
	// anyArray matches the PostgreSQL spellings "col = ANY (ARRAY['a', 'b'])"
	// and "col = ANY ('{a,b}')".
	anyArray = regexp.MustCompile(`(?i)` + boundary + `(` + ident + `)\s*(?:=|<>|!=)\s*(?:any|all)\s*\(\s*(?:array\s*\[\s*` + literals + `\s*\]|'\{[^}]*\}'(?:::\w+(?:\[\])?)?)\s*\)`)
	// comparison matches one "col = 'a'" or "col <> 'a'" term; two or more
	// on the same column spell out a value list.
	comparison = regexp.MustCompile(`(?i)` + boundary + `(` + ident + `)\s*(=|<>|!=)\s*` + literal + `(?:$|[\s)])`)
)

// Scan returns every enum-style CHECK constraint and enum type in src.
// Keywords inside SQL comments or string literals are ignored.
func Scan(src string) []Finding {
	var findings []Finding
	masked := maskCommentsAndStrings(src)
	for _, loc := range checkKeyword.FindAllStringIndex(masked, -1) {
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
		findings = append(findings, Finding{Kind: CheckConstraint, Offset: loc[0], Line: line, Column: col, ColumnName: column, Expr: expr})
	}
	for _, m := range enumKeyword.FindAllStringSubmatchIndex(masked, -1) {
		open := m[1] - 1
		end := matchParen(src, open)
		if end < 0 {
			continue
		}
		line, col := position(src, m[0])
		findings = append(findings, Finding{Kind: EnumType, Offset: m[0], Line: line, Column: col, ColumnName: src[m[2]:m[3]], Expr: strings.TrimSpace(src[open+1 : end])})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Offset < findings[j].Offset })
	return findings
}

// enumeratedColumn reports the first column whose allowed values expr spells
// out, anywhere inside the expression: a literal IN list, a PostgreSQL ANY
// array, or two or more equality (or inequality) comparisons against
// literals on the same column.
func enumeratedColumn(expr string) (string, bool) {
	normalized := whitespace.ReplaceAllString(expr, " ")
	for _, re := range []*regexp.Regexp{inList, anyArray} {
		if m := re.FindStringSubmatch(normalized); m != nil {
			return columnName(m[1]), true
		}
	}
	counts := map[string]int{}
	for _, m := range comparison.FindAllStringSubmatch(normalized, -1) {
		key := columnName(m[1])
		if m[2] != "=" {
			key = "<>" + key
		}
		counts[key]++
		if counts[key] >= 2 {
			return strings.TrimPrefix(key, "<>"), true
		}
	}
	return "", false
}

// columnName strips a lower()/upper() wrapper from a matched ident.
func columnName(ident string) string {
	if i := strings.IndexByte(ident, '('); i >= 0 {
		ident = strings.TrimSuffix(strings.TrimSpace(ident[i+1:]), ")")
	}
	return strings.TrimSpace(ident)
}

// maskCommentsAndStrings blanks line comments, block comments, and
// single-quoted strings with spaces so keyword searches skip them. Offsets
// and newlines are preserved, so positions map back to src.
func maskCommentsAndStrings(src string) string {
	out := []byte(src)
	blank := func(from, to int) {
		for i := from; i < to && i < len(out); i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
	for i := 0; i < len(src); {
		switch {
		case src[i] == '\'':
			end := i + 1
			for end < len(src) {
				if src[end] == '\'' {
					if end+1 < len(src) && src[end+1] == '\'' {
						end += 2
						continue
					}
					break
				}
				end++
			}
			blank(i, end+1)
			i = end + 1
		case strings.HasPrefix(src[i:], "--"):
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				end = len(src) - i
			}
			blank(i, i+end)
			i += end
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				end = len(src) - i
			} else {
				end += 4
			}
			blank(i, i+end)
			i += end
		default:
			i++
		}
	}
	return string(out)
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

// Analyzer reports enum-style CHECK constraints and enum types inside Go
// string literals in non-test files, where embedded schema and migration SQL
// usually lives.
var Analyzer = &analysis.Analyzer{
	Name:     "sqlenum",
	Doc:      "reports SQL CHECK constraints and enum types that hard-code a column's allowed values",
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
