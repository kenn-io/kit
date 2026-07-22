package gitworktree

import "strings"

// PorcelainEntry is one block from `git worktree list --porcelain`.
type PorcelainEntry struct {
	Path           string
	Head           string
	Branch         string
	Bare           bool
	Detached       bool
	Locked         bool
	LockedReason   string
	Prunable       bool
	PrunableReason string
}

// ParsePorcelain parses `git worktree list --porcelain` output. Unknown fields
// are ignored so newer Git versions can extend the format compatibly.
func ParsePorcelain(output string) []PorcelainEntry {
	blocks := strings.Split(strings.TrimSpace(output), "\n\n")
	entries := make([]PorcelainEntry, 0, len(blocks))
	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var entry PorcelainEntry
		for line := range strings.SplitSeq(block, "\n") {
			line = strings.TrimSuffix(line, "\r")
			switch {
			case strings.HasPrefix(line, "worktree "):
				entry.Path = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			case strings.HasPrefix(line, "HEAD "):
				entry.Head = strings.TrimSpace(strings.TrimPrefix(line, "HEAD "))
			case strings.HasPrefix(line, "branch "):
				entry.Branch = strings.TrimPrefix(
					strings.TrimSpace(strings.TrimPrefix(line, "branch ")),
					"refs/heads/",
				)
			case line == "bare":
				entry.Bare = true
			case line == "detached":
				entry.Detached = true
			case line == "locked":
				entry.Locked = true
			case strings.HasPrefix(line, "locked "):
				entry.Locked = true
				entry.LockedReason = strings.TrimSpace(strings.TrimPrefix(line, "locked "))
			case line == "prunable":
				entry.Prunable = true
			case strings.HasPrefix(line, "prunable "):
				entry.Prunable = true
				entry.PrunableReason = strings.TrimSpace(strings.TrimPrefix(line, "prunable "))
			}
		}
		if entry.Path != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}
