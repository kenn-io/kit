package rrf

import (
	"errors"
	"fmt"
	"slices"
)

// Contribution is one leg's evidence for a fused key or group.
type Contribution struct {
	Leg    string
	Rank   int
	Weight float64
	Term   float64
}

// Hit is a fused key. Contributions follow leg order.
type Hit[K comparable] struct {
	Key           K
	Score         float64
	Contributions []Contribution
}

// Leg is one ranked list, best key first. A repeated key keeps its earliest
// rank and does not contribute twice.
type Leg[K comparable] struct {
	Name   string
	Weight float64
	Keys   []K
}

// Fuse combines legs. k must be positive. Every leg needs a unique name and
// a positive weight. The result is every key that appeared, highest score
// first. Fuse does not apply a result limit; callers limit after eligibility.
func Fuse[K comparable](k float64, legs []Leg[K]) ([]Hit[K], error) {
	if k <= 0 {
		return nil, errors.New("rrf: k must be positive")
	}
	if err := validateLegs(len(legs), func(i int) (string, float64) {
		return legs[i].Name, legs[i].Weight
	}); err != nil {
		return nil, err
	}
	type state struct {
		hit   Hit[K]
		first int
	}
	order := make([]K, 0)
	states := make(map[K]*state)
	seq := 0
	for _, leg := range legs {
		seen := make(map[K]struct{}, len(leg.Keys))
		rank := 0
		for _, key := range leg.Keys {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			rank++
			term := leg.Weight / (k + float64(rank))
			item := states[key]
			if item == nil {
				seq++
				item = &state{hit: Hit[K]{Key: key}, first: seq}
				states[key] = item
				order = append(order, key)
			}
			item.hit.Score += term
			item.hit.Contributions = append(item.hit.Contributions, Contribution{
				Leg: leg.Name, Rank: rank, Weight: leg.Weight, Term: term,
			})
		}
	}
	slices.SortStableFunc(order, func(a, b K) int {
		left, right := states[a], states[b]
		if left.hit.Score > right.hit.Score {
			return -1
		}
		if left.hit.Score < right.hit.Score {
			return 1
		}
		return left.first - right.first
	})
	hits := make([]Hit[K], len(order))
	for i, key := range order {
		hits[i] = states[key].hit
	}
	return hits, nil
}

// Alternate is one member of a group found by one leg.
type Alternate[M comparable] struct {
	Member M
	Leg    string
}

// GroupHit is a fused group. Alternates are retained in discovery order and
// are not deduplicated across legs. Eligibility is the caller's decision.
type GroupHit[G, M comparable] struct {
	Group         G
	Score         float64
	Contributions []Contribution
	Alternates    []Alternate[M]
}

// Group is one ranked group and the members observed in that occurrence.
type Group[G, M comparable] struct {
	Key     G
	Members []M
}

// GroupLeg is one ranked list of groups, best group first.
type GroupLeg[G, M comparable] struct {
	Name   string
	Weight float64
	Groups []Group[G, M]
}

// FuseGroups ranks each group once per leg and keeps every alternate member.
// A later duplicate of the same group in one leg does not add another
// contribution; its members are appended.
func FuseGroups[G, M comparable](k float64, legs []GroupLeg[G, M]) ([]GroupHit[G, M], error) {
	if k <= 0 {
		return nil, errors.New("rrf: k must be positive")
	}
	if err := validateLegs(len(legs), func(i int) (string, float64) {
		return legs[i].Name, legs[i].Weight
	}); err != nil {
		return nil, err
	}
	type state struct {
		hit   GroupHit[G, M]
		first int
	}
	order := make([]G, 0)
	states := make(map[G]*state)
	seq := 0
	for _, leg := range legs {
		seen := make(map[G]struct{}, len(leg.Groups))
		rank := 0
		for _, group := range leg.Groups {
			_, ranked := seen[group.Key]
			if !ranked {
				seen[group.Key] = struct{}{}
				rank++
			}
			item := states[group.Key]
			if item == nil {
				seq++
				item = &state{hit: GroupHit[G, M]{Group: group.Key}, first: seq}
				states[group.Key] = item
				order = append(order, group.Key)
			}
			if !ranked {
				term := leg.Weight / (k + float64(rank))
				item.hit.Score += term
				item.hit.Contributions = append(item.hit.Contributions, Contribution{
					Leg: leg.Name, Rank: rank, Weight: leg.Weight, Term: term,
				})
			}
			for _, member := range group.Members {
				item.hit.Alternates = append(item.hit.Alternates, Alternate[M]{Member: member, Leg: leg.Name})
			}
		}
	}
	slices.SortStableFunc(order, func(a, b G) int {
		left, right := states[a], states[b]
		if left.hit.Score > right.hit.Score {
			return -1
		}
		if left.hit.Score < right.hit.Score {
			return 1
		}
		return left.first - right.first
	})
	hits := make([]GroupHit[G, M], len(order))
	for i, key := range order {
		hits[i] = states[key].hit
	}
	return hits, nil
}

func validateLegs(n int, leg func(int) (string, float64)) error {
	names := make(map[string]struct{}, n)
	for i := range n {
		name, weight := leg(i)
		if name == "" {
			return errors.New("rrf: leg name is required")
		}
		if _, ok := names[name]; ok {
			return fmt.Errorf("rrf: duplicate leg %q", name)
		}
		names[name] = struct{}{}
		if weight <= 0 {
			return fmt.Errorf("rrf: leg %q weight must be positive", name)
		}
	}
	return nil
}
