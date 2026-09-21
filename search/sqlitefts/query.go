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

// Option configures a Helper. Table and key options are required. The rank
// function defaults to bm25.
type Option func(*Helper)

// Helper builds candidate queries for one full-text index and the source
// table it maps to. Names are identifiers, are quoted by the builder, and
// must be simple names. SourceKey must uniquely identify a source row.
// IndexKey is the FTS column containing SourceKey values; use "rowid" for
// rowid mapping.
type Helper struct {
	indexTable   string
	indexKey     string
	sourceTable  string
	sourceKey    string
	rankFunction string
}

// WithIndexTable sets the FTS5 table name.
func WithIndexTable(name string) Option {
	return func(h *Helper) { h.indexTable = name }
}

// WithIndexKey sets the FTS column that stores the source key.
func WithIndexKey(name string) Option {
	return func(h *Helper) { h.indexKey = name }
}

// WithSourceTable sets the source table joined to the FTS index.
func WithSourceTable(name string) Option {
	return func(h *Helper) { h.sourceTable = name }
}

// WithSourceKey sets the source column matched to IndexKey.
func WithSourceKey(name string) Option {
	return func(h *Helper) { h.sourceKey = name }
}

// WithRankFunction sets the SQLite rank function. The default is bm25.
// The function is called with the FTS table's real name, which is the form
// SQLite uses for bm25 and MATCH. The score is the negation of that
// function, so a custom function is treated as lower-is-better, like bm25.
func WithRankFunction(name string) Option {
	return func(h *Helper) { h.rankFunction = name }
}

// New validates the index mapping and returns a helper. The rank function
// is bm25 unless WithRankFunction sets another identifier.
func New(opts ...Option) (Helper, error) {
	h := Helper{rankFunction: "bm25"}
	for _, opt := range opts {
		if opt == nil {
			return Helper{}, errors.New("sqlitefts: nil option")
		}
		opt(&h)
	}
	for name, value := range map[string]string{
		"index table": h.indexTable, "index key": h.indexKey,
		"source table": h.sourceTable, "source key": h.sourceKey,
		"rank function": h.rankFunction,
	} {
		if !validIdentifier(value) {
			return Helper{}, fmt.Errorf("sqlitefts: invalid %s %q", name, value)
		}
	}
	return h, nil
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
	Match           string
	SourcePredicate Predicate
	ExtraSourceCols []Column
	Limit           int
}

// Build returns a mapped FTS5 candidate SELECT. Scores are higher-is-better
// (the negation of the rank function, bm25 by default); ties are ordered by
// source id. Output columns are doc_key, score, then the requested extra
// columns. Match is prepared FTS5 syntax, not automatically escaped literal
// text. Outer queries must specify their own ordering.
func (h Helper) Build(req Request) (sqlquery.Query, error) {
	if err := h.valid(); err != nil {
		return sqlquery.Query{}, err
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
		"d." + quote(h.sourceKey) + " AS doc_key",
		"-" + h.rankFunction + "(" + quote(h.indexTable) + ") AS score",
	}
	for _, col := range req.ExtraSourceCols {
		projection = append(projection, "d."+quote(col.Name)+" AS "+quote(col.As))
	}

	var b strings.Builder
	joinKey := "f." + quote(h.indexKey)
	fmt.Fprintf(&b, "SELECT %s FROM %s AS f JOIN %s AS d ON d.%s = %s WHERE %s MATCH ?",
		strings.Join(projection, ", "), quote(h.indexTable), quote(h.sourceTable), quote(h.sourceKey), joinKey, quote(h.indexTable))
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

func (h Helper) valid() error {
	for name, value := range map[string]string{
		"index table": h.indexTable, "index key": h.indexKey,
		"source table": h.sourceTable, "source key": h.sourceKey,
		"rank function": h.rankFunction,
	} {
		if !validIdentifier(value) {
			return fmt.Errorf("sqlitefts: invalid %s %q", name, value)
		}
	}
	return nil
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
