// Package sqlcheck reports CHECK constraints and enum types in SQL schema
// and migration text.
//
// A CHECK constraint bakes a validation rule into the schema: every change to
// the rule, such as a new allowed value for a status column, needs a migration
// that drops and recreates the constraint (and on SQLite, often the whole
// table). The same goes for CREATE TYPE ... AS ENUM. Rules belong in
// application code or in lookup tables, where they can change without a
// schema migration, so the package reports every occurrence.
//
// The package scans raw SQL (migration files) and, through Analyzer, SQL held
// in Go string literals.
package sqlcheck

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
	// CheckConstraint is a CHECK (...) constraint.
	CheckConstraint Kind = iota
	// EnumType is a CREATE TYPE ... AS ENUM (...) declaration.
	EnumType
)

// Finding is one CHECK constraint or enum type in a SQL source.
type Finding struct {
	// Kind says whether the finding is a CHECK constraint or an enum type.
	Kind Kind
	// Offset is the byte offset of the CHECK or CREATE TYPE keyword.
	Offset int
	// Line and Column are 1-based positions of the keyword.
	Line, Column int
	// Name is the enum type name, or the first column the constraint
	// mentions (empty when none can be identified).
	Name string
	// Expr is the constraint expression or the enum value list as written.
	Expr string
}

// Message renders the diagnostic for a finding.
func (f Finding) Message() string {
	if f.Kind == EnumType {
		return fmt.Sprintf("enum type %s locks its allowed values into the schema; every new value then needs a migration to alter the type. Store the column as text and validate the set in application code or a lookup table", f.Name)
	}
	subject := "CHECK constraint"
	if f.Name != "" {
		subject += " on " + f.Name
	}
	return subject + " locks a validation rule into the schema; every change to the rule then needs a schema migration to rewrite the constraint. Validate in application code or keep allowed values in a lookup table"
}

var (
	checkKeyword = regexp.MustCompile(`(?i)\bCHECK\s*\(`)
	enumKeyword  = regexp.MustCompile(`(?i)\bCREATE\s+TYPE\s+(` + name + `)\s+AS\s+ENUM\s*\(`)
	whitespace   = regexp.MustCompile(`\s+`)

	// name is a possibly quoted or qualified identifier.
	name = `(?:"[^"]+"|` + "`[^`]+`" + `|\[[^\]]+\]|[A-Za-z_]\w*)(?:\.(?:"[^"]+"|\w+))*`
	// firstColumn finds the first bare identifier in a constraint expression
	// that is not a SQL keyword or a function call.
	firstColumn = regexp.MustCompile(`(?i)(?:^|[\s(])(` + name + `)(?:$|[\s),=<>!])`)

	keywords = map[string]bool{
		"not": true, "null": true, "and": true, "or": true, "in": true, "is": true,
		"between": true, "like": true, "true": true, "false": true, "any": true,
		"all": true, "array": true, "select": true, "exists": true, "case": true,
		"when": true, "then": true, "else": true, "end": true, "glob": true,
		"escape": true, "some": true,
	}
)

// Scan returns every CHECK constraint and enum type in src, in source order.
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
		line, col := position(src, loc[0])
		findings = append(findings, Finding{Kind: CheckConstraint, Offset: loc[0], Line: line, Column: col, Name: columnName(expr), Expr: expr})
	}
	for _, m := range enumKeyword.FindAllStringSubmatchIndex(masked, -1) {
		open := m[1] - 1
		end := matchParen(src, open)
		if end < 0 {
			continue
		}
		line, col := position(src, m[0])
		findings = append(findings, Finding{Kind: EnumType, Offset: m[0], Line: line, Column: col, Name: src[m[2]:m[3]], Expr: strings.TrimSpace(src[open+1 : end])})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Offset < findings[j].Offset })
	return findings
}

// columnName returns the first column an expression mentions, for the
// diagnostic. Function names and keywords are skipped.
func columnName(expr string) string {
	normalized := whitespace.ReplaceAllString(maskCommentsAndStrings(expr), " ")
	for _, m := range firstColumn.FindAllStringSubmatchIndex(normalized, -1) {
		candidate := normalized[m[2]:m[3]]
		if keywords[strings.ToLower(candidate)] {
			continue
		}
		if rest := strings.TrimLeft(normalized[m[3]:], " "); strings.HasPrefix(rest, "(") {
			continue // function call
		}
		return candidate
	}
	return ""
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

// Analyzer reports CHECK constraints and enum types inside Go string
// literals in non-test files, where embedded schema and migration SQL
// usually lives.
var Analyzer = &analysis.Analyzer{
	Name:     "sqlcheck",
	Doc:      "reports SQL CHECK constraints and enum types, which lock validation rules into the schema",
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
