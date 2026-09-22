package hybrid

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kit/search/rrf"
	"go.kenn.io/kit/search/sqlquery"
)

// Leg is one retrieval query in rank order. Scan reads the current row and
// returns the fusion key. CandidateLimit is the bound the caller placed in
// Query. Zero means the bound is unknown and the window is not reported full.
type Leg[K comparable] struct {
	Name           string
	Weight         float64
	Query          sqlquery.Query
	CandidateLimit int
	Scan           func(*sql.Rows) (K, error)
}

// GroupLeg scans a group key and one alternate member from each row.
type GroupLeg[G, M comparable] struct {
	Name           string
	Weight         float64
	Query          sqlquery.Query
	CandidateLimit int
	Scan           func(*sql.Rows) (G, M, error)
}

// Report describes one executed leg. FullWindow means the raw candidate
// window was filled before source filters, or, when the backend does not
// say, that the returned row count reached the candidate limit. It does
// not mean the source has no further matches.
type Report struct {
	Name           string
	Returned       int
	CandidateLimit int
	FullWindow     bool
}

// Result is the fused ranking plus per-leg window metadata.
type Result[K comparable] struct {
	Hits []rrf.Hit[K]
	Legs []Report
}

// GroupResult keeps alternate members for a later eligibility check.
type GroupResult[G, M comparable] struct {
	Hits []rrf.GroupHit[G, M]
	Legs []Report
}

// Run executes legs sequentially through db and fuses their key order.
// k is the reciprocal-rank constant. Legs stop when ctx is cancelled.
// A scan or query error returns no fused result.
func Run[K comparable](ctx context.Context, db sqlquery.Queryer, k float64, legs []Leg[K]) (Result[K], error) {
	if db == nil {
		return Result[K]{}, errors.New("hybrid: database handle is required")
	}
	ranked := make([]rrf.Leg[K], len(legs))
	reports := make([]Report, len(legs))
	for i, leg := range legs {
		if leg.Scan == nil {
			return Result[K]{}, fmt.Errorf("hybrid: leg %q has no scanner", leg.Name)
		}
		if leg.CandidateLimit < 0 {
			return Result[K]{}, fmt.Errorf("hybrid: leg %q candidate limit is negative", leg.Name)
		}
		keys, err := scanKeys(ctx, db, leg.Query, func(rows *sql.Rows) (K, error) { return leg.Scan(rows) })
		if err != nil {
			return Result[K]{}, fmt.Errorf("hybrid: leg %q: %w", leg.Name, err)
		}
		ranked[i] = rrf.Leg[K]{Name: leg.Name, Weight: leg.Weight, Keys: keys}
		full, err := windowFull(ctx, db, leg.Query, leg.CandidateLimit, len(keys))
		if err != nil {
			return Result[K]{}, fmt.Errorf("hybrid: leg %q window: %w", leg.Name, err)
		}
		reports[i] = report(leg.Name, leg.CandidateLimit, len(keys), full)
	}
	hits, err := rrf.Fuse(k, ranked)
	if err != nil {
		return Result[K]{}, err
	}
	return Result[K]{Hits: hits, Legs: reports}, nil
}

// RunGroups executes legs that return group and member columns. Members stay
// attached to the fused group so a caller can drop ineligible alternates
// before choosing a representative.
func RunGroups[G, M comparable](ctx context.Context, db sqlquery.Queryer, k float64, legs []GroupLeg[G, M]) (GroupResult[G, M], error) {
	if db == nil {
		return GroupResult[G, M]{}, errors.New("hybrid: database handle is required")
	}
	ranked := make([]rrf.GroupLeg[G, M], len(legs))
	reports := make([]Report, len(legs))
	for i, leg := range legs {
		if leg.Scan == nil {
			return GroupResult[G, M]{}, fmt.Errorf("hybrid: leg %q has no scanner", leg.Name)
		}
		if leg.CandidateLimit < 0 {
			return GroupResult[G, M]{}, fmt.Errorf("hybrid: leg %q candidate limit is negative", leg.Name)
		}
		type row struct {
			group  G
			member M
		}
		rows, err := scanKeys(ctx, db, leg.Query, func(rows *sql.Rows) (row, error) {
			group, member, err := leg.Scan(rows)
			return row{group: group, member: member}, err
		})
		if err != nil {
			return GroupResult[G, M]{}, fmt.Errorf("hybrid: leg %q: %w", leg.Name, err)
		}
		groups := make([]rrf.Group[G, M], 0, len(rows))
		for _, item := range rows {
			if len(groups) == 0 || groups[len(groups)-1].Key != item.group {
				groups = append(groups, rrf.Group[G, M]{Key: item.group})
			}
			last := len(groups) - 1
			groups[last].Members = append(groups[last].Members, item.member)
		}
		ranked[i] = rrf.GroupLeg[G, M]{Name: leg.Name, Weight: leg.Weight, Groups: groups}
		full, err := windowFull(ctx, db, leg.Query, leg.CandidateLimit, len(rows))
		if err != nil {
			return GroupResult[G, M]{}, fmt.Errorf("hybrid: leg %q window: %w", leg.Name, err)
		}
		reports[i] = report(leg.Name, leg.CandidateLimit, len(rows), full)
	}
	hits, err := rrf.FuseGroups(k, ranked)
	if err != nil {
		return GroupResult[G, M]{}, err
	}
	return GroupResult[G, M]{Hits: hits, Legs: reports}, nil
}

func report(name string, limit, returned int, full bool) Report {
	return Report{
		Name:           name,
		Returned:       returned,
		CandidateLimit: limit,
		FullWindow:     full,
	}
}

func windowFull(ctx context.Context, db sqlquery.Queryer, query sqlquery.Query, limit, returned int) (bool, error) {
	if query.RawWindow != nil {
		return query.RawWindow(ctx, db)
	}
	return limit > 0 && returned >= limit, nil
}

func scanKeys[T any](ctx context.Context, db sqlquery.Queryer, query sqlquery.Query, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := db.QueryContext(ctx, query.SQL, query.Args...)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []T
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan search row: %w", err)
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read search rows: %w", err)
	}
	return out, nil
}
