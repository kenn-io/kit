package packstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sort"
	"time"

	"go.kenn.io/kit/pack"
)

const (
	defaultRepackMinAge    = 24 * time.Hour
	defaultRepackDeadBytes = int64(8 << 20)
)

// RepackSelection configures sparse-pack and small-pack eligibility. Zero
// MinAge selects 24 hours and zero MinDeadStored selects 8 MiB.
type RepackSelection struct {
	MinAge        time.Duration
	MinDeadStored int64
	// MergeBelowStored merges live packs whose stored bytes are below it, and
	// that are at least MinAge old, into packs of up to TargetSize live stored
	// bytes. Packs merge in creation order, in groups of at least two sources.
	// A positive MaxBytes admits whole groups while their live raw bytes fit
	// the rest of the budget, and always admits the run's first rewrite.
	// Zero disables merging.
	MergeBelowStored int64
}

// RepackOptions bounds and dates one reclamation run. MaxBytes greater than
// zero selects bounded automatic behavior that continues past source-content
// failures; zero selects fail-fast explicit behavior.
type RepackOptions struct {
	TargetSize int64
	MaxBytes   int64
	Now        time.Time
	Selection  RepackSelection
}

// RepackStats summarizes committed source-independent reclamation.
type RepackStats struct {
	MappingsPruned int64
	// PacksSelected counts empty and sparse sources; PacksMerged counts small
	// sources replaced by merging.
	PacksSelected          int
	PacksMerged            int
	PacksRewritten         int
	PacksSealed            int
	PacksRemoved           int
	PacksDeferredOversized int
	BlobsRepacked          int
	BytesRepacked          int64
	BudgetExhausted        bool
}

// Repack retires zero-live packs first, then rewrites each selected sparse
// source independently through an exact catalog compare-and-swap.
func (m *Maintainer) Repack(ctx context.Context, opts RepackOptions) (RepackStats, error) {
	var stats RepackStats
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	if opts.TargetSize < 0 || opts.MaxBytes < 0 || opts.Selection.MinAge < 0 || opts.Selection.MinDeadStored < 0 ||
		opts.Selection.MergeBelowStored < 0 {
		return stats, ErrInvalidPolicy
	}
	lease, err := m.coordinator.AcquireMaintenance(ctx)
	if err != nil {
		return stats, err
	}
	defer func() { _ = lease.Release() }()
	pruned, err := m.catalog.PruneUnreferenced(ctx)
	if err != nil {
		return stats, err
	}
	stats.MappingsPruned = pruned
	usage, err := m.catalog.ListPackUsage(ctx)
	if err != nil {
		return stats, err
	}
	selected := selectRepackSources(usage, opts)
	stats.PacksSelected = len(selected)
	automatic := opts.MaxBytes > 0
	var runErr error
	var partial []PackUsage
	for _, source := range selected {
		if source.LiveEntries != 0 {
			partial = append(partial, source)
			continue
		}
		if err := m.retireEmpty(ctx, source.PackID, &stats); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}
	for _, source := range partial {
		if err := ctx.Err(); err != nil {
			return stats, errors.Join(runErr, err)
		}
		if automatic && stats.BytesRepacked >= opts.MaxBytes {
			stats.BudgetExhausted = true
			break
		}
		if source.MaxLiveRawLen > m.limits.BlobBytes || source.MaxLiveStoredLen > m.limits.BlobBytes {
			stats.PacksDeferredOversized++
			continue
		}
		entries, err := m.catalog.ListLivePackEntries(ctx, source.PackID)
		if err != nil {
			return stats, errors.Join(runErr, err)
		}
		if err := verifyUsage(source, entries); err != nil {
			return stats, errors.Join(runErr, err)
		}
		if err := m.preflightRepackSource(source.PackID, entries); err != nil {
			err = fmt.Errorf("packstore: repack source %s: %w", source.PackID, err)
			if errors.Is(err, ErrBlobTooLarge) {
				stats.PacksDeferredOversized++
				continue
			}
			if automatic && isSourceContentError(err) {
				runErr = errors.Join(runErr, err)
				continue
			}
			return stats, errors.Join(runErr, err)
		}
		result, err := m.rewriteSources(ctx, entries, opts.TargetSize)
		if err != nil {
			err = fmt.Errorf("packstore: repack source %s: %w", source.PackID, err)
			if errors.Is(err, ErrBlobTooLarge) {
				stats.PacksDeferredOversized++
				continue
			}
			if automatic && isSourceContentError(err) {
				runErr = errors.Join(runErr, err)
				continue
			}
			return stats, errors.Join(runErr, err)
		}
		if err := m.catalog.CommitRepack(ctx, []string{source.PackID}, result.records, result.moves); err != nil {
			return stats, errors.Join(runErr, err)
		}
		stats.PacksRewritten++
		stats.PacksSealed += len(result.records)
		stats.BlobsRepacked += len(result.moves)
		stats.BytesRepacked += result.rawBytes
		if err := m.retireEmpty(ctx, source.PackID, &stats); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}
	if stats.BudgetExhausted || opts.Selection.MergeBelowStored == 0 {
		return stats, runErr
	}
	for _, group := range selectMergeGroups(usage, selected, opts) {
		if err := ctx.Err(); err != nil {
			return stats, errors.Join(runErr, err)
		}
		// Whole groups keep merged outputs near the target size; the first may
		// exceed the budget so that a small budget still makes progress.
		if automatic && stats.BytesRepacked > 0 && stats.BytesRepacked+liveRawBytes(group) > opts.MaxBytes {
			stats.BudgetExhausted = true
			break
		}
		if err := m.mergeGroup(ctx, group, opts, &stats, &runErr); err != nil {
			return stats, errors.Join(runErr, err)
		}
	}
	return stats, runErr
}

// mergeGroup replaces small live sources with packs of their combined live
// entries. Like sparse rewriting, automatic runs skip sources whose content
// fails and report it with the run's error.
func (m *Maintainer) mergeGroup(
	ctx context.Context, group []PackUsage, opts RepackOptions, stats *RepackStats, runErr *error,
) error {
	automatic := opts.MaxBytes > 0
	var sourceIDs []string
	var entries []IndexEntry
	for _, source := range group {
		if source.MaxLiveRawLen > m.limits.BlobBytes || source.MaxLiveStoredLen > m.limits.BlobBytes {
			stats.PacksDeferredOversized++
			continue
		}
		live, err := m.catalog.ListLivePackEntries(ctx, source.PackID)
		if err != nil {
			return err
		}
		if err := verifyUsage(source, live); err != nil {
			return err
		}
		if err := m.preflightRepackSource(source.PackID, live); err != nil {
			err = fmt.Errorf("packstore: merge source %s: %w", source.PackID, err)
			if errors.Is(err, ErrBlobTooLarge) {
				stats.PacksDeferredOversized++
				continue
			}
			if automatic && isSourceContentError(err) {
				*runErr = errors.Join(*runErr, err)
				continue
			}
			return err
		}
		sourceIDs = append(sourceIDs, source.PackID)
		entries = append(entries, live...)
	}
	var result rewriteResult
	for {
		if len(sourceIDs) < 2 {
			return nil
		}
		var err error
		result, err = m.rewriteSources(ctx, entries, opts.TargetSize)
		if err == nil {
			break
		}
		err = fmt.Errorf("packstore: merge sources: %w", err)
		if errors.Is(err, ErrBlobTooLarge) {
			stats.PacksDeferredOversized += len(sourceIDs)
			return nil
		}
		if !automatic || !isSourceContentError(err) {
			return err
		}
		*runErr = errors.Join(*runErr, err)
		// Merge the remaining sources so one damaged pack cannot block the
		// rest of its group on every pass.
		failed, ok := errors.AsType[*sourceReadError](err)
		if !ok || !slices.Contains(sourceIDs, failed.packID) {
			return nil
		}
		sourceIDs = slices.DeleteFunc(sourceIDs, func(id string) bool { return id == failed.packID })
		entries = slices.DeleteFunc(entries, func(entry IndexEntry) bool { return entry.PackID == failed.packID })
	}
	if err := m.catalog.CommitRepack(ctx, sourceIDs, result.records, result.moves); err != nil {
		return err
	}
	stats.PacksMerged += len(sourceIDs)
	stats.PacksSealed += len(result.records)
	stats.BlobsRepacked += len(result.moves)
	stats.BytesRepacked += result.rawBytes
	for _, packID := range sourceIDs {
		if err := m.retireEmpty(ctx, packID, stats); err != nil {
			*runErr = errors.Join(*runErr, err)
		}
	}
	return nil
}

// selectMergeGroups groups old small live packs, in creation order, so each
// group's live stored bytes fit one target-size pack. Groups of one source
// would only rewrite it, so they are dropped.
func selectMergeGroups(usage []PackUsage, selected []PackUsage, opts RepackOptions) [][]PackUsage {
	target := opts.TargetSize
	if target == 0 {
		target = pack.DefaultTargetSize
	}
	cutoff := repackNow(opts).Add(-repackMinAge(opts))
	excluded := make(map[string]struct{}, len(selected))
	for _, source := range selected {
		excluded[source.PackID] = struct{}{}
	}
	var groups [][]PackUsage
	var current []PackUsage
	var currentStored int64
	flush := func() {
		if len(current) >= 2 {
			groups = append(groups, current)
		}
		current, currentStored = nil, 0
	}
	for _, source := range orderedUsage(usage) {
		if _, skip := excluded[source.PackID]; skip || source.LiveEntries == 0 ||
			source.StoredBytes >= opts.Selection.MergeBelowStored || source.CreatedAt.After(cutoff) {
			continue
		}
		if len(current) > 0 && currentStored+source.LiveStoredBytes > target {
			flush()
		}
		current = append(current, source)
		currentStored += source.LiveStoredBytes
	}
	flush()
	return groups
}

func liveRawBytes(group []PackUsage) int64 {
	var total int64
	for _, source := range group {
		total += source.LiveRawBytes
	}
	return total
}

func repackNow(opts RepackOptions) time.Time {
	if opts.Now.IsZero() {
		return time.Now().UTC()
	}
	return opts.Now.UTC()
}

func repackMinAge(opts RepackOptions) time.Duration {
	if opts.Selection.MinAge == 0 {
		return defaultRepackMinAge
	}
	return opts.Selection.MinAge
}

func orderedUsage(usage []PackUsage) []PackUsage {
	ordered := append([]PackUsage(nil), usage...)
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
		}
		return ordered[i].PackID < ordered[j].PackID
	})
	return ordered
}

func selectRepackSources(usage []PackUsage, opts RepackOptions) []PackUsage {
	now := repackNow(opts)
	minAge := repackMinAge(opts)
	minDead := opts.Selection.MinDeadStored
	if minDead == 0 {
		minDead = defaultRepackDeadBytes
	}
	var zero, partial []PackUsage
	for _, source := range orderedUsage(usage) {
		if source.LiveEntries == 0 {
			zero = append(zero, source)
			continue
		}
		deadStored := source.StoredBytes - source.LiveStoredBytes
		belowHalf := source.EntryCount > 0 && source.LiveEntries <= (source.EntryCount-1)/2
		oldEnough := !source.CreatedAt.After(now.Add(-minAge))
		if belowHalf && oldEnough && deadStored >= minDead {
			partial = append(partial, source)
		}
	}
	return append(zero, partial...)
}

func verifyUsage(source PackUsage, entries []IndexEntry) error {
	var stored, raw int64
	for _, entry := range entries {
		stored += entry.StoredLen
		raw += entry.RawLen
	}
	if int64(len(entries)) != source.LiveEntries || stored != source.LiveStoredBytes || raw != source.LiveRawBytes {
		return fmt.Errorf("packstore: source %s live inventory changed during selection", source.PackID)
	}
	return nil
}

func (m *Maintainer) preflightRepackSource(packID string, entries []IndexEntry) error {
	reader, err := OpenMaintenancePack(m.layout.PackPath(packID), m.limits)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	footer := make(map[Hash]pack.Entry)
	for _, entry := range reader.Entries() {
		hash, parseErr := ParseHash(entry.ID.String())
		if parseErr != nil {
			return parseErr
		}
		footer[hash] = entry
	}
	for _, indexed := range entries {
		authoritative, ok := footer[indexed.Hash]
		if !ok || indexed.PackID != packID || !packIndexMatchesFooter(&indexed, authoritative) {
			return fmt.Errorf("%w: source %s metadata mismatch for %s", pack.ErrCorrupt, packID, indexed.Hash)
		}
		if authoritative.RawLen > uint64(m.limits.BlobBytes) {
			return newLimitError(LimitBlobRawBytes, authoritative.RawLen, uint64(m.limits.BlobBytes))
		}
		if authoritative.StoredLen > uint64(m.limits.BlobBytes) {
			return newLimitError(LimitBlobStoredBytes, authoritative.StoredLen, uint64(m.limits.BlobBytes))
		}
	}
	return nil
}

type rewriteResult struct {
	records  []PackRecord
	moves    []RepackMove
	rawBytes int64
}

// sourceReadError reports a live entry that could not be read from its source
// pack, so a merge can leave that source out.
type sourceReadError struct {
	packID string
	hash   Hash
	err    error
}

func (e *sourceReadError) Error() string {
	return fmt.Sprintf("packstore: read source %s blob %s: %v", e.packID, e.hash, e.err)
}

func (e *sourceReadError) Unwrap() error { return e.err }

type rewriteSourceEntry struct {
	indexed IndexEntry
	sealed  pack.Entry
}

// rewriteSources copies live entries, each from the source pack its PackID
// names, into replacement packs. Each stored frame is copied as is after its
// CRC32C, decoded length, and SHA-256 verify; frames above
// maxCopiedFrameBytes are recompressed through PrepareBlob instead.
func (m *Maintainer) rewriteSources(ctx context.Context, entries []IndexEntry, targetSize int64) (rewriteResult, error) {
	var result rewriteResult
	var writer *pack.Writer
	var current []rewriteSourceEntry
	abort := func() {
		if writer != nil {
			_ = writer.Abort()
		}
	}
	defer abort()
	seal := func() error {
		if writer == nil {
			return nil
		}
		packID := writer.ID()
		path := m.layout.PackPath(packID)
		sealed, err := writer.Seal(path)
		if err != nil {
			return err
		}
		if err := validateSealedOutput(path, m.limits); err != nil {
			return err
		}
		if len(sealed) != len(current) {
			return errors.New("packstore: replacement entry count changed")
		}
		record := PackRecord{PackID: packID, EntryCount: int64(len(sealed)), CreatedAt: time.Now().UTC()}
		for i, entry := range sealed {
			if entry.ID.String() != current[i].indexed.Hash.String() {
				return fmt.Errorf("%w: replacement hash mismatch", ErrContentMismatch)
			}
			newEntry := indexFromPack(packID, entry)
			record.StoredBytes += newEntry.StoredLen
			result.moves = append(result.moves, RepackMove{OldPackID: current[i].indexed.PackID, NewEntry: newEntry})
		}
		result.records = append(result.records, record)
		writer = nil
		current = nil
		return nil
	}
	// Sources are read under the store's read policy, as packed streams are.
	sources := newSourceFrames(m.layout, m.store.limits)
	defer sources.Close()
	for _, indexed := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		source, err := sources.entry(indexed)
		if err != nil {
			return result, &sourceReadError{packID: indexed.PackID, hash: indexed.Hash, err: err}
		}
		// Copy the verified stored frame when it fits in memory; recompress
		// larger ones through bounded disk scratch.
		var frame []byte
		var prepared *pack.PreparedBlob
		storedLen := source.StoredLen
		if source.StoredLen <= maxCopiedFrameBytes {
			frame, err = sources.readVerified(ctx, source)
			if err != nil {
				return result, &sourceReadError{packID: indexed.PackID, hash: indexed.Hash, err: err}
			}
		} else {
			prepared, err = sources.prepare(ctx, indexed, source, m.layout.PacksDir())
			if err != nil {
				return result, &sourceReadError{packID: indexed.PackID, hash: indexed.Hash, err: err}
			}
			if prepared == nil {
				return result, fmt.Errorf("packstore: prepare source %s blob %s returned no result", indexed.PackID, indexed.Hash)
			}
			storedLen = prepared.StoredLen()
		}
		discard := func() {
			if prepared != nil {
				_ = prepared.Close()
			}
		}
		if err := checkPlainOutput(m.limits, uint64(pack.MinEntryOffset), storedLen, 1); err != nil {
			discard()
			return result, err
		}
		if writer != nil {
			if err := checkPlainOutput(m.limits, uint64(writer.StoredSize()), storedLen, len(current)+1); err != nil {
				if err := seal(); err != nil {
					discard()
					return result, err
				}
			}
		}
		if writer == nil {
			writer, err = pack.NewWriter(m.layout.PacksDir(), pack.WriterOptions{TargetSize: targetSize})
			if err != nil {
				discard()
				return result, err
			}
		}
		var sealed pack.Entry
		if prepared != nil {
			sealed, err = writer.AppendPrepared(ctx, prepared)
		} else {
			sealed, err = writer.AppendEncoded(source.ID, frame, source.RawLen, source.Flags&pack.BlobCompressed != 0)
		}
		if err != nil {
			return result, err
		}
		current = append(current, rewriteSourceEntry{indexed: indexed, sealed: sealed})
		result.rawBytes += indexed.RawLen
		if writer.Full() {
			if err := seal(); err != nil {
				return result, err
			}
		}
	}
	return result, seal()
}

func (m *Maintainer) retireEmpty(ctx context.Context, packID string, stats *RepackStats) error {
	deleted, err := m.catalog.DeleteEmptyPackRecord(ctx, packID)
	if err != nil {
		return err
	}
	if !deleted {
		return fmt.Errorf("packstore: pack %s still has mappings", packID)
	}
	if err := m.filesystem.Retire(ctx, ObjectRef{PackID: packID}); err != nil {
		return err
	}
	stats.PacksRemoved++
	return nil
}

func isSourceContentError(err error) bool {
	for _, known := range []error{
		fs.ErrNotExist, pack.ErrBadMagic, pack.ErrUnsupportedVersion,
		pack.ErrTruncated, pack.ErrChecksum, pack.ErrCorrupt, pack.ErrBlobMismatch, ErrContentMismatch,
	} {
		if errors.Is(err, known) {
			return true
		}
	}
	pathErr, ok := errors.AsType[*os.PathError](err)
	return ok && errors.Is(pathErr, fs.ErrNotExist)
}
