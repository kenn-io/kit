package packstoretest_test

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"testing"
	"time"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/kit/packstore/packstoretest"
)

type memoryHarness struct {
	catalog *memoryCatalog
}

func newMemoryHarness(*testing.T) packstoretest.CatalogHarness {
	return &memoryHarness{catalog: &memoryCatalog{
		members:    make(map[packstore.Hash]bool),
		candidates: make(map[packstore.Hash]packstore.Candidate),
		entries:    make(map[packstore.Hash]packstore.IndexEntry),
		packs:      make(map[string]packstore.PackRecord),
	}}
}

func (h *memoryHarness) Catalog() packstore.Catalog { return h.catalog }

func (h *memoryHarness) SetMember(hash packstore.Hash, member bool) {
	h.catalog.members[hash] = member
}

func (h *memoryHarness) SetCandidate(candidate packstore.Candidate) {
	h.catalog.candidates[candidate.Hash] = candidate
}

func (h *memoryHarness) PutPack(record packstore.PackRecord, entries []packstore.IndexEntry) {
	h.catalog.packs[record.PackID] = record
	for _, entry := range entries {
		h.catalog.entries[entry.Hash] = entry
	}
}

func (h *memoryHarness) Snapshot() packstoretest.CatalogState {
	state := packstoretest.CatalogState{
		Members: make(map[packstore.Hash]bool, len(h.catalog.members)),
		Entries: make(map[packstore.Hash]packstore.IndexEntry, len(h.catalog.entries)),
		Packs:   make(map[string]packstore.PackRecord, len(h.catalog.packs)),
	}
	maps.Copy(state.Members, h.catalog.members)
	maps.Copy(state.Entries, h.catalog.entries)
	maps.Copy(state.Packs, h.catalog.packs)
	return state
}

type memoryCatalog struct {
	members    map[packstore.Hash]bool
	candidates map[packstore.Hash]packstore.Candidate
	entries    map[packstore.Hash]packstore.IndexEntry
	packs      map[string]packstore.PackRecord
}

func (c *memoryCatalog) Resolve(_ context.Context, hash packstore.Hash) (packstore.Location, error) {
	entry, ok := c.entries[hash]
	if !c.members[hash] {
		return packstore.Location{}, nil
	}
	if !ok {
		return packstore.Location{Member: true}, nil
	}
	return packstore.Location{Member: true, Pack: &entry}, nil
}

func (c *memoryCatalog) ListReferences(context.Context) ([]packstore.Reference, error) {
	refs := make([]packstore.Reference, 0, len(c.members))
	for hash, member := range c.members {
		if member {
			refs = append(refs, packstore.Reference{Hash: hash, OriginalHashes: []string{hash.String()}})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Hash < refs[j].Hash })
	return refs, nil
}

func (c *memoryCatalog) ListUnpacked(context.Context) ([]packstore.Candidate, error) {
	var candidates []packstore.Candidate
	for hash, candidate := range c.candidates {
		if c.members[hash] {
			if _, packed := c.entries[hash]; !packed {
				candidates = append(candidates, candidate)
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Hash < candidates[j].Hash })
	return candidates, nil
}

func (c *memoryCatalog) ListIndexed(context.Context) ([]packstore.IndexEntry, error) {
	entries := make([]packstore.IndexEntry, 0, len(c.entries))
	for _, entry := range c.entries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Hash < entries[j].Hash })
	return entries, nil
}

func (c *memoryCatalog) ListPackRecords(context.Context) ([]packstore.PackRecord, error) {
	records := make([]packstore.PackRecord, 0, len(c.packs))
	for _, record := range c.packs {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].PackID < records[j].PackID })
	return records, nil
}

func (c *memoryCatalog) ListPackEntries(_ context.Context, packID string) ([]packstore.IndexEntry, error) {
	var entries []packstore.IndexEntry
	for _, entry := range c.entries {
		if entry.PackID == packID {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func (c *memoryCatalog) HasPackRecord(_ context.Context, packID string) (bool, error) {
	_, ok := c.packs[packID]
	return ok, nil
}

func (c *memoryCatalog) PruneUnreferenced(context.Context) (int64, error) {
	var pruned int64
	for hash := range c.entries {
		if !c.members[hash] {
			delete(c.entries, hash)
			pruned++
		}
	}
	return pruned, nil
}

func (c *memoryCatalog) RecordPack(_ context.Context, record packstore.PackRecord, adoptions []packstore.Adoption) error {
	if _, exists := c.packs[record.PackID]; exists {
		return fmt.Errorf("pack already exists")
	}
	c.packs[record.PackID] = record
	for _, adoption := range adoptions {
		if c.members[adoption.Entry.Hash] {
			if _, exists := c.entries[adoption.Entry.Hash]; !exists {
				c.entries[adoption.Entry.Hash] = adoption.Entry
			}
		}
	}
	return nil
}

func (c *memoryCatalog) AdoptPack(_ context.Context, record packstore.PackRecord, adoptions []packstore.Adoption) error {
	c.packs[record.PackID] = record
	for _, adoption := range adoptions {
		if c.members[adoption.Entry.Hash] {
			c.entries[adoption.Entry.Hash] = adoption.Entry
		}
	}
	return nil
}

func (c *memoryCatalog) DeletePackRecord(_ context.Context, packID string) error {
	for hash, entry := range c.entries {
		if entry.PackID == packID {
			delete(c.entries, hash)
		}
	}
	delete(c.packs, packID)
	return nil
}

func (c *memoryCatalog) DeleteIndexEntry(_ context.Context, hash packstore.Hash) error {
	delete(c.entries, hash)
	return nil
}

func (c *memoryCatalog) ListPackUsage(context.Context) ([]packstore.PackUsage, error) {
	usage := make([]packstore.PackUsage, 0, len(c.packs))
	for _, record := range c.packs {
		u := packstore.PackUsage{PackRecord: record}
		for hash, entry := range c.entries {
			if entry.PackID == record.PackID && c.members[hash] {
				u.LiveEntries++
				u.LiveStoredBytes += entry.StoredLen
				u.LiveRawBytes += entry.RawLen
				u.MaxLiveStoredLen = max(u.MaxLiveStoredLen, entry.StoredLen)
				u.MaxLiveRawLen = max(u.MaxLiveRawLen, entry.RawLen)
			}
		}
		usage = append(usage, u)
	}
	return usage, nil
}

func (c *memoryCatalog) ListLivePackEntries(_ context.Context, packID string) ([]packstore.IndexEntry, error) {
	var entries []packstore.IndexEntry
	for hash, entry := range c.entries {
		if entry.PackID == packID && c.members[hash] {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func (c *memoryCatalog) CommitRepack(_ context.Context, sourceIDs []string, records []packstore.PackRecord, moves []packstore.RepackMove) error {
	expected := make(map[packstore.Hash]string)
	for _, sourceID := range sourceIDs {
		for hash, entry := range c.entries {
			if entry.PackID == sourceID && c.members[hash] {
				expected[hash] = sourceID
			}
		}
	}
	if len(expected) != len(moves) {
		return fmt.Errorf("move set is not exact")
	}
	for _, move := range moves {
		if expected[move.NewEntry.Hash] != move.OldPackID {
			return fmt.Errorf("move set is not exact")
		}
	}
	for _, record := range records {
		c.packs[record.PackID] = record
	}
	for _, move := range moves {
		c.entries[move.NewEntry.Hash] = move.NewEntry
	}
	return nil
}

func (c *memoryCatalog) DeleteEmptyPackRecord(_ context.Context, packID string) (bool, error) {
	for _, entry := range c.entries {
		if entry.PackID == packID {
			return false, nil
		}
	}
	if _, exists := c.packs[packID]; !exists {
		return false, nil
	}
	delete(c.packs, packID)
	return true, nil
}

func (c *memoryCatalog) ClearPackMetadata(context.Context) error {
	clear(c.entries)
	clear(c.packs)
	return nil
}

func TestMemoryCatalogConforms(t *testing.T) {
	packstoretest.RunCatalogContract(t, newMemoryHarness, packstoretest.ContractOptions{
		Now: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
		NewPackID: func() string {
			return pack.NewPackID()
		},
	})
}
