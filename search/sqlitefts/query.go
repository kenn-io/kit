// Package sqlitefts builds composable, mapped SQLite FTS5 candidate queries.
//
// Query text is passed to SQLite's MATCH operator as a parameter. Callers are
// responsible for preparing literal or intentional FTS5 syntax according to
// their chosen tokenizer and query semantics.
package sqlitefts

import (
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kit/search/sqlquery"
)

// Mapping identifies the FTS table and the source table it maps to. All names
// are identifiers, are quoted by the builder, and must be simple names (not
// SQL expressions). SourceKey must uniquely identify a source row. IndexKey
// is the FTS column containing SourceKey values; use "rowid" for rowid mapping.
type Mapping struct {
	IndexTable  string
	IndexKey    string
	SourceTable string
	SourceKey   string
}

// Column projects a source column under an explicit result alias.
type Column struct {
	Name string
	As   string
}

// Predicate is a trusted SQL fragment over source alias d and its positional
// arguments. The fragment is placed inside the candidate query before ORDER
// BY and LIMIT. Since source rows always use the fixed alias d, predicates
// should refer to columns as d.<column> and use anonymous ? placeholders.
// User values belong in Args, never in SQL.
type Predicate struct {
	SQL  string
	Args []any
}

// Request describes one bounded FTS5 candidate query.
type Request struct {
	Mapping         Mapping
	Match           string
	SourcePredicate Predicate
	ExtraSourceCols []Column
	Limit           int
}

// Build returns a mapped FTS5 candidate SELECT. Scores are higher-is-better
// (the negation of SQLite's lower-is-better bm25 value); ties are ordered by
// source id. Output columns are doc_key, score, then the requested extra
// columns. Match is prepared FTS5 syntax, not automatically escaped literal
// text. Outer queries must specify their own ordering.
func Build(req Request) (sqlquery.Query, error) {
	m := req.Mapping
	for name, value := range map[string]string{
		"index table": m.IndexTable, "index key": m.IndexKey, "source table": m.SourceTable, "source key": m.SourceKey,
	} {
		if !validIdentifier(value) {
			return sqlquery.Query{}, fmt.Errorf("sqlitefts: invalid %s %q", name, value)
		}
	}
	if req.Limit <= 0 {
		return sqlquery.Query{}, errors.New("sqlitefts: limit must be positive")
	}
	aliases := map[string]bool{"doc_key": true, "score": true}
	for _, col := range req.ExtraSourceCols {
		if !validIdentifier(col.Name) || !validIdentifier(col.As) || aliases[strings.ToLower(col.As)] {
			return sqlquery.Query{}, fmt.Errorf("sqlitefts: invalid or conflicting extra source column %q AS %q", col.Name, col.As)
		}
		aliases[strings.ToLower(col.As)] = true
	}

	projection := []string{
		"d." + quote(m.SourceKey) + " AS doc_key",
		"-bm25(" + quote(m.IndexTable) + ") AS score",
	}
	for _, col := range req.ExtraSourceCols {
		projection = append(projection, "d."+quote(col.Name)+" AS "+quote(col.As))
	}

	var b strings.Builder
	joinKey := "f." + quote(m.IndexKey)
	fmt.Fprintf(&b, "SELECT %s FROM %s AS f JOIN %s AS d ON d.%s = %s WHERE %s MATCH ?",
		strings.Join(projection, ", "), quote(m.IndexTable), quote(m.SourceTable), quote(m.SourceKey), joinKey, quote(m.IndexTable))
	args := []any{req.Match}
	if strings.TrimSpace(req.SourcePredicate.SQL) != "" {
		b.WriteString(" AND (")
		b.WriteString(req.SourcePredicate.SQL)
		b.WriteString(")")
		args = append(args, req.SourcePredicate.Args...)
	} else if len(req.SourcePredicate.Args) != 0 {
		return sqlquery.Query{}, errors.New("sqlitefts: source predicate args require SQL")
	}
	b.WriteString(" ORDER BY score DESC, doc_key ASC LIMIT ?")
	args = append(args, req.Limit)
	return sqlquery.Query{SQL: b.String(), Args: args}, nil
}

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
