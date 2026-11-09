package packstore

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kit/internal/contextio"
	"go.kenn.io/kit/pack"
)

// PackOptions bounds one packing run. MaxBytes is a soft committed raw-byte
// budget; zero is unlimited.
type PackOptions struct {
	TargetSize int64
	MaxBytes   int64
	// MaxVerifyBytes bounds existing packed-content checks by the larger of
	// raw and stored bytes, allowing one entry to exceed the remaining budget.
	// Zero examines every indexed entry, reading packed bytes for entries with
	// a loose repair source. Positive values resume across calls on
	// this Maintainer, up to the last entry present when the sweep started.
	// A pass that fails or is canceled keeps the progress it made, except
	// that it resumes before any repair it found but did not commit.
	// A new Maintainer starts a fresh sweep.
	MaxVerifyBytes int64
	// MinSealBytes holds loose members until their combined logical size
	// reaches it or the oldest reaches MaxLooseAge, so frequent passes do not
	// seal one small pack each. Zero packs whatever is loose on every pass.
	// Members that packing skips, such as oversized or missing ones, count
	// toward neither limit, nor do members an earlier pass of this
	// Maintainer could not read. Repairs of damaged packed entries are never
	// held.
	MinSealBytes int64
	// MaxLooseAge seals loose members below MinSealBytes once the oldest
	// loose file was modified at least this long ago. It applies only when
	// MinSealBytes is positive; zero then selects 24 hours.
	MaxLooseAge time.Duration
	// Now dates loose ages. Zero uses the current time.
	Now time.Time
}

const defaultMaxLooseAge = 24 * time.Hour

// PackStats summarizes committed and repair outcomes.
type PackStats struct {
	// BlobsChecked counts indexed entries examined, including those whose packed bytes are skipped.
	BlobsChecked int
	// VerificationPending reports an unfinished sweep. Entries beyond its
	// captured end wait for the next one.
	VerificationPending        bool
	PacksSealed                int
	BlobsPacked                int
	BytesPacked                int64
	PacksAdopted               int
	PacksRemoved               int
	PacksQuarantined           int
	PacksUnreadable            int
	RecordsDropped             int
	MappingsPruned             int64
	BlobsMissing               int
	BlobsCorrupt               int
	BlobsDeferredOversized     int
	PacksDeferredOversized     int
	LooseSwept                 int
	LooseOrphansRemoved        int
	LooseOrphansDeferred       int
	LooseOrphanSweepSuppressed bool
	// LooseSealDeferred reports loose members left for a later pass because
	// they were below MinSealBytes and younger than MaxLooseAge.
	LooseSealDeferred bool
	BudgetExhausted   bool
}

// Pack repairs inventory, reconciles orphan packs, packs loose members, and
// sweeps verified redundant loose files in that order.
func (m *Maintainer) Pack(ctx context.Context, opts PackOptions) (PackStats, error) {
	var stats PackStats
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	if opts.TargetSize < 0 || opts.MaxBytes < 0 || opts.MaxVerifyBytes < 0 ||
		opts.MinSealBytes < 0 || opts.MaxLooseAge < 0 {
		return stats, ErrInvalidPolicy
	}
	lease, err := m.coordinator.AcquireMaintenance(ctx)
	if err != nil {
		return stats, err
	}
	defer func() { _ = lease.Release() }()
	progress := verificationProgress{through: m.verifiedThrough, safe: m.verifiedThrough, end: m.verificationEnd}
	defer func() { m.verifiedThrough, m.verificationEnd = progress.resume() }()
	if err := pack.MkdirAllSynced(m.layout.PacksDir()); err != nil {
		return stats, err
	}
	if err := m.cleanPackStaging(ctx); err != nil {
		return stats, err
	}
	inventory, err := m.catalog.ListReferences(ctx)
	if err != nil {
		return stats, err
	}
	refs := make(map[Hash]Reference, len(inventory.References))
	for _, reference := range inventory.References {
		refs[reference.Hash] = reference
	}
	recoveries, err := m.dropDangling(ctx, &stats, refs, opts.MaxVerifyBytes, &progress)
	if err != nil {
		return stats, err
	}
	if err := m.packCandidates(ctx, recoveries, opts, &stats, packCandidatePolicy{
		commit:          m.catalog.AdoptPack,
		openPin:         m.openVerificationPin,
		replaceMappings: true,
	}); err != nil {
		return stats, err
	}
	// A packing budget can defer some verified repair candidates as well.
	if !stats.BudgetExhausted {
		progress.repairPending = false
	}
	pruned, err := m.catalog.PruneUnreferenced(ctx)
	if err != nil {
		return stats, err
	}
	stats.MappingsPruned += pruned
	if inventory.Complete {
		if err := m.reconcileOrphans(ctx, refs, &stats); err != nil {
			return stats, err
		}
	}
	if !stats.BudgetExhausted {
		if err := m.packLoose(ctx, opts, &stats); err != nil {
			return stats, err
		}
	}
	if err := m.sweepLoose(ctx, refs, inventory.Complete, &stats); err != nil {
		return stats, err
	}
	return stats, nil
}

// verificationProgress tracks one pass through a bounded verification sweep.
// Entries at or before through were examined; safe is the last of them
// examined before the first repair this pass found.
type verificationProgress struct {
	through       IndexEntry
	safe          IndexEntry
	end           IndexEntry
	repairPending bool
	finished      bool
}

// resume returns the sweep bounds for the next pass. A repair that was found
// but not committed must be found again, so the next pass starts before it.
func (p *verificationProgress) resume() (through, end IndexEntry) {
	switch {
	case p.repairPending:
		return p.safe, p.end
	case p.finished:
		return IndexEntry{}, IndexEntry{}
	default:
		return p.through, p.end
	}
}

func (m *Maintainer) cleanPackStaging(ctx context.Context) error {
	entries, err := os.ReadDir(m.layout.PacksDir())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".staging") {
			if err := os.Remove(filepath.Join(m.layout.PacksDir(), entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Maintainer) dropDangling(
	ctx context.Context, stats *PackStats, refs map[Hash]Reference, maxVerifyBytes int64, progress *verificationProgress,
) ([]Candidate, error) {
	records, err := m.catalog.ListPackRecords(ctx)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := os.Stat(m.layout.PackPath(record.PackID)); errors.Is(err, fs.ErrNotExist) {
			if err := m.catalog.DeletePackRecord(ctx, record.PackID); err != nil {
				return nil, err
			}
			stats.RecordsDropped++
		} else if err != nil {
			return nil, err
		}
	}
	indexed, err := m.catalog.ListIndexed(ctx)
	if err != nil {
		return nil, err
	}
	// Physical ordering avoids reopening packs for hash-interleaved entries.
	slices.SortFunc(indexed, compareVerificationEntry)
	if maxVerifyBytes > 0 && progress.end == (IndexEntry{}) && len(indexed) > 0 {
		// New packs must not keep a sweep from returning to older entries.
		progress.end = indexed[len(indexed)-1]
	}
	var recoveries []Candidate
	remaining := maxVerifyBytes
	for _, entry := range indexed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if maxVerifyBytes > 0 {
			if compareVerificationEntry(entry, progress.end) > 0 {
				break
			}
			if compareVerificationEntry(entry, progress.through) <= 0 {
				continue
			}
		}
		if maxVerifyBytes > 0 && remaining == 0 {
			stats.VerificationPending = true
			return recoveries, nil
		}
		recovery, err := m.repairIndexed(ctx, stats, refs, entry)
		if err != nil {
			return nil, err
		}
		if recovery != nil {
			recoveries = append(recoveries, *recovery)
			progress.repairPending = true
		}
		stats.BlobsChecked++
		if maxVerifyBytes > 0 {
			remaining = max(0, remaining-max(1, entry.RawLen, entry.StoredLen))
			progress.through = entry
			if !progress.repairPending {
				progress.safe = entry
			}
		}
	}
	progress.finished = true
	return recoveries, nil
}

func compareVerificationEntry(a, b IndexEntry) int {
	return cmp.Or(cmp.Compare(a.PackID, b.PackID), cmp.Compare(a.Offset, b.Offset), cmp.Compare(a.Hash, b.Hash))
}

func (m *Maintainer) repairIndexed(ctx context.Context, stats *PackStats, refs map[Hash]Reference, entry IndexEntry) (*Candidate, error) {
	if _, err := os.Stat(m.layout.PackPath(entry.PackID)); errors.Is(err, fs.ErrNotExist) {
		if err := m.catalog.DeleteIndexEntry(ctx, entry.Hash); err != nil {
			return nil, err
		}
		stats.MappingsPruned++
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	reference, live := refs[entry.Hash]
	if !live {
		return nil, nil
	}
	// A damaged entry without a loose copy has nothing to repair from.
	if candidate, err := m.hasCanonicalLooseCandidate(ctx, entry.Hash); err == nil && !candidate {
		return nil, nil
	}
	stream, _, err := m.store.openPackedStream(ctx, entry.Hash, &entry)
	if err == nil {
		_, err = io.Copy(io.Discard, stream)
		err = errors.Join(err, stream.Close())
	}
	if err == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	logicalSize, valid, err := m.findAnyValidCanonicalLoose(ctx, entry.Hash)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, nil
	}
	// Keep the unreadable mapping authoritative until the verified replacement
	// is sealed and AdoptPack atomically advances the catalog.
	return &Candidate{
		Hash: entry.Hash, OriginalHashes: append([]string(nil), reference.OriginalHashes...),
		Paths: []string{m.layout.CompressedLoosePath(entry.Hash), m.layout.LoosePath(entry.Hash)}, Size: logicalSize,
	}, nil
}

func (m *Maintainer) reconcileOrphans(ctx context.Context, refs map[Hash]Reference, stats *PackStats) error {
	// One snapshot answers for every file in the walk. Maintenance records
	// packs only under the lease this pass holds, and a restore must already
	// exclude maintenance because its packs are uncataloged until it commits.
	records, err := m.catalog.ListPackRecords(ctx)
	if err != nil {
		return err
	}
	recorded := make(map[string]struct{}, len(records))
	for _, record := range records {
		recorded[record.PackID] = struct{}{}
	}
	return filepath.WalkDir(m.layout.PacksDir(), func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), PackExt) {
			return nil
		}
		packID := strings.TrimSuffix(entry.Name(), PackExt)
		if !pack.IsValidPackID(packID) || filepath.Clean(path) != filepath.Clean(m.layout.PackPath(packID)) {
			return nil
		}
		if _, ok := recorded[packID]; ok {
			return nil
		}
		return m.reconcileOne(ctx, path, packID, refs, stats)
	})
}

func (m *Maintainer) reconcileOne(ctx context.Context, path, packID string, refs map[Hash]Reference, stats *PackStats) error {
	reader, err := OpenMaintenancePack(path, m.limits)
	if err != nil {
		if errors.Is(err, ErrBlobTooLarge) {
			stats.PacksDeferredOversized++
			return nil
		}
		stats.PacksUnreadable++
		return nil
	}
	readerOpen := true
	defer func() {
		if readerOpen {
			_ = reader.Close()
		}
	}()
	footer := reader.Entries()
	record := PackRecord{PackID: packID, EntryCount: int64(len(footer)), CreatedAt: time.Now().UTC()}
	adoptions := make([]Adoption, 0, len(footer))
	for _, footerEntry := range footer {
		if err := ctx.Err(); err != nil {
			return err
		}
		record.StoredBytes += int64(footerEntry.StoredLen)
		hash, err := ParseHash(footerEntry.ID.String())
		if err != nil {
			return err
		}
		reference, live := refs[hash]
		if !live {
			continue
		}
		if footerEntry.RawLen > uint64(m.limits.BlobBytes) || footerEntry.StoredLen > uint64(m.limits.BlobBytes) {
			stats.PacksDeferredOversized++
			return nil
		}
		location, err := m.catalog.Resolve(ctx, hash)
		if err != nil {
			return err
		}
		if location.Pack != nil && location.Pack.PackID != packID {
			if _, _, readErr := m.store.ReadBounded(ctx, hash, m.limits.BlobBytes); readErr == nil {
				continue
			}
		}
		if location.Member && location.Pack == nil {
			valid, verifyErr := m.hasAuthoritativeCanonicalLoose(ctx, hash)
			if verifyErr != nil {
				return verifyErr
			}
			if valid {
				continue
			}
		}
		stream, err := reader.OpenBlob(ctx, hash)
		if err != nil {
			stats.PacksQuarantined++
			return nil
		}
		if err := errors.Join(stream.Verify(), stream.Close()); err != nil {
			stats.PacksQuarantined++
			return nil
		}
		adoptions = append(adoptions, Adoption{Entry: indexFromPack(packID, footerEntry), OriginalHashes: reference.OriginalHashes})
	}
	if err := reader.Close(); err != nil {
		return err
	}
	readerOpen = false
	if len(adoptions) == 0 {
		if err := os.Remove(path); err != nil {
			return err
		}
		stats.PacksRemoved++
		return nil
	}
	if err := m.catalog.AdoptPack(ctx, record, adoptions); err != nil {
		return err
	}
	stats.PacksAdopted++
	return nil
}

type packedSource struct {
	candidate Candidate
	path      string
	pin       identityPin
	entry     pack.Entry
}

type identityPin interface {
	Stat() (fs.FileInfo, error)
	Close() error
	removeClaim(string) error
}

type verificationOnlyIdentityPin struct {
	looseVerificationIdentityPin
}

func (*verificationOnlyIdentityPin) removeClaim(string) error {
	return fmt.Errorf("packstore: verification-only identity pin cannot remove content: %w", errors.ErrUnsupported)
}

type identityPinOpener func(string) (identityPin, fs.FileInfo, error)

var removePinnedLooseClaim = func(pin identityPin, path string) error {
	return pin.removeClaim(path)
}

// openLooseIdentityPin linearizes namespace-only removal at pin acquisition.
// Callers that verified an earlier descriptor must separately compare its
// identity with the returned pin before authorizing content cleanup.
func openLooseIdentityPin(path string) (identityPin, fs.FileInfo, error) {
	pin, pinned, err := openLooseRemovalIdentityPin(path)
	if err != nil {
		if current, snapshotErr := snapshotPathIdentity(path); snapshotErr == nil {
			if validationErr := validateRegularNoFollow(path, current); validationErr != nil {
				return nil, nil, errors.Join(validationErr, err)
			}
		}
		return nil, nil, err
	}
	if pin == nil || pinned == nil {
		return nil, nil, errors.Join(fmt.Errorf("packstore: identity pin for %s has incomplete state", path), closeIdentityPin(pin))
	}
	if err := validateRegularNoFollow(path, pinned); err != nil {
		return nil, nil, errors.Join(err, pin.Close())
	}
	return pin, pinned, nil
}

func openLooseMaintenanceVerificationPin(path string) (identityPin, fs.FileInfo, error) {
	pin, identity, err := openLooseVerificationIdentityPin(path)
	if err != nil {
		return nil, nil, err
	}
	return &verificationOnlyIdentityPin{looseVerificationIdentityPin: pin}, identity, nil
}

type candidateGroup struct {
	Candidate
	err error
}

func mergeLogicalCandidates(candidates []Candidate) []candidateGroup {
	groups := make([]candidateGroup, 0, len(candidates))
	byHash := make(map[Hash]int, len(candidates))
	pathSets := make([]map[string]struct{}, 0, len(candidates))
	aliasSets := make([]map[string]struct{}, 0, len(candidates))
	for _, candidate := range candidates {
		index, exists := byHash[candidate.Hash]
		if !exists {
			index = len(groups)
			byHash[candidate.Hash] = index
			groups = append(groups, candidateGroup{
				Hash: candidate.Hash,
				Size: candidate.Size,
			})
			pathSets = append(pathSets, make(map[string]struct{}, len(candidate.Paths)))
			aliasSets = append(aliasSets, make(map[string]struct{}, len(candidate.OriginalHashes)))
		} else if groups[index].Size != candidate.Size {
			groups[index].err = errors.Join(groups[index].err, fmt.Errorf(
				"%w: candidate %s has contradictory logical sizes %d and %d",
				ErrContentMismatch, candidate.Hash, groups[index].Size, candidate.Size,
			))
		}
		for _, path := range candidate.Paths {
			if _, duplicate := pathSets[index][path]; duplicate {
				continue
			}
			pathSets[index][path] = struct{}{}
			groups[index].Paths = append(groups[index].Paths, path)
		}
		for _, alias := range candidate.OriginalHashes {
			if _, duplicate := aliasSets[index][alias]; duplicate {
				continue
			}
			aliasSets[index][alias] = struct{}{}
			groups[index].OriginalHashes = append(groups[index].OriginalHashes, alias)
		}
	}
	return groups
}

func (m *Maintainer) packLoose(ctx context.Context, opts PackOptions, stats *PackStats) (resultErr error) {
	candidates, err := m.catalog.ListUnpacked(ctx)
	if err != nil {
		return err
	}
	m.forgetPackedLooseSkips(candidates)
	if len(candidates) == 0 {
		return nil
	}
	ready, err := m.looseReadyToSeal(ctx, candidates, opts, stats)
	if err != nil {
		return err
	}
	if !ready {
		stats.LooseSealDeferred = true
		return nil
	}
	return m.packCandidates(ctx, candidates, opts, stats, packCandidatePolicy{
		commit:       m.catalog.RecordPack,
		openPin:      m.openVerificationPin,
		removeSource: true,
		noteSkip:     m.noteLooseSkip,
	})
}

// looseSkip records why packing could not use a loose member.
type looseSkip uint8

const (
	looseUsable looseSkip = iota
	looseCorrupt
	looseOversized
)

func (s looseSkip) count(stats *PackStats) {
	switch s {
	case looseCorrupt:
		stats.BlobsCorrupt++
	case looseOversized:
		stats.BlobsDeferredOversized++
	case looseUsable:
	}
}

// forgetPackedLooseSkips drops remembered members that are no longer loose.
func (m *Maintainer) forgetPackedLooseSkips(candidates []Candidate) {
	if len(m.looseSkips) == 0 {
		return
	}
	loose := make(map[Hash]struct{}, len(candidates))
	for _, candidate := range candidates {
		loose[candidate.Hash] = struct{}{}
	}
	maps.DeleteFunc(m.looseSkips, func(hash Hash, _ looseRejection) bool {
		_, still := loose[hash]
		return !still
	})
}

// looseRejection is a member whose content packing read and could not use,
// with what readiness saw of it at the time. A later change, such as a repair,
// a new copy, or a catalog fix, makes it eligible again.
type looseRejection struct {
	skip  looseSkip
	stamp looseStamp
}

// noteLooseSkip remembers or forgets a loose member that packing examined.
func (m *Maintainer) noteLooseSkip(ctx context.Context, group candidateGroup, skip looseSkip) {
	if skip == looseUsable {
		delete(m.looseSkips, group.Hash)
		return
	}
	stamp, err := statLoose(ctx, m.layout.Root(), group)
	if err != nil {
		// Without a stamp a repair could not be noticed, so do not remember it.
		delete(m.looseSkips, group.Hash)
		return
	}
	if m.looseSkips == nil {
		m.looseSkips = make(map[Hash]looseRejection)
	}
	m.looseSkips[group.Hash] = looseRejection{skip: skip, stamp: stamp}
}

// looseReadyToSeal reports whether loose members justify a new pack under
// opts. Members that packing always skips (oversized, conflicting sizes, with
// no existing path, or rejected by an earlier pass of this Maintainer) never
// make a pass ready, so they cannot force a small pack on every pass. When it
// holds the pass, it counts them in stats as packing would.
func (m *Maintainer) looseReadyToSeal(
	ctx context.Context, candidates []Candidate, opts PackOptions, stats *PackStats,
) (bool, error) {
	if opts.MinSealBytes == 0 {
		return true, nil
	}
	maxAge := cmp.Or(opts.MaxLooseAge, defaultMaxLooseAge)
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	sealBefore := now.Add(-maxAge)
	var skipped PackStats
	var total int64
	// Packing reads each hash from the union of its rows' paths, so judge
	// readiness on the same merged view.
	for _, group := range mergeLogicalCandidates(candidates) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		stamp, err := statLoose(ctx, m.layout.Root(), group)
		if err != nil {
			return false, err
		}
		// Only packing reads loose content, so a damaged file that still
		// exists is known as unusable once packing has rejected it, until the
		// member or its file changes.
		if rejected, known := m.looseSkips[group.Hash]; known {
			if rejected.stamp.equal(stamp) {
				rejected.skip.count(&skipped)
				continue
			}
			delete(m.looseSkips, group.Hash)
		}
		if group.err != nil || group.Size < 0 {
			skipped.BlobsCorrupt++
			continue
		}
		if group.Size > m.limits.BlobBytes {
			skipped.BlobsDeferredOversized++
			continue
		}
		file, exists := stamp.firstFile()
		if !exists {
			skipped.BlobsMissing++
			continue
		}
		total += group.Size
		if total >= opts.MinSealBytes || !file.modTime.After(sealBefore) {
			return true, nil
		}
	}
	stats.BlobsCorrupt += skipped.BlobsCorrupt
	stats.BlobsDeferredOversized += skipped.BlobsDeferredOversized
	stats.BlobsMissing += skipped.BlobsMissing
	return false, nil
}

// looseStamp is what readiness sees of a loose member without reading it: its
// cataloged size, whether its rows conflict, and the state of every path.
type looseStamp struct {
	size     int64
	conflict bool
	files    []looseFileState
}

type looseFileState struct {
	path    string
	exists  bool
	length  int64
	modTime time.Time
}

func (s looseStamp) equal(other looseStamp) bool {
	return s.size == other.size && s.conflict == other.conflict &&
		slices.EqualFunc(s.files, other.files, func(a, b looseFileState) bool {
			return a.path == b.path && a.exists == b.exists && a.length == b.length && a.modTime.Equal(b.modTime)
		})
}

// firstFile returns the first existing file, which packing would read first.
func (s looseStamp) firstFile() (looseFileState, bool) {
	index := slices.IndexFunc(s.files, func(file looseFileState) bool { return file.exists })
	if index < 0 {
		return looseFileState{}, false
	}
	return s.files[index], true
}

func statLoose(ctx context.Context, root string, group candidateGroup) (looseStamp, error) {
	stamp := looseStamp{size: group.Size, conflict: group.err != nil, files: make([]looseFileState, 0, len(group.Paths))}
	for _, path := range group.Paths {
		if err := ctx.Err(); err != nil {
			return looseStamp{}, err
		}
		file := looseFileState{path: resolveCandidatePath(root, path)}
		if info, err := os.Lstat(file.path); err == nil {
			file.exists, file.length, file.modTime = true, info.Size(), info.ModTime()
		}
		stamp.files = append(stamp.files, file)
	}
	return stamp, nil
}

// resolveCandidatePath resolves a catalog candidate path, which may be
// relative to the store root.
func resolveCandidatePath(root, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, filepath.FromSlash(path))
	}
	return filepath.Clean(path)
}

type packCatalogCommit func(context.Context, PackRecord, []Adoption) error

type packCandidatePolicy struct {
	commit          packCatalogCommit
	openPin         identityPinOpener
	replaceMappings bool
	removeSource    bool
	// noteSkip, when set, learns which candidates packing could use.
	noteSkip func(context.Context, candidateGroup, looseSkip)
}

func (m *Maintainer) packCandidates(
	ctx context.Context,
	candidates []Candidate,
	opts PackOptions,
	stats *PackStats,
	policy packCandidatePolicy,
) (resultErr error) {
	var writer *pack.Writer
	var sources []packedSource
	rawBytes := stats.BytesPacked
	abort := func() error {
		if writer != nil {
			return writer.Abort()
		}
		return nil
	}
	defer func() {
		resultErr = errors.Join(resultErr, abort(), closePackedSourcePins(sources))
	}()
	seal := func() error {
		if writer == nil || len(sources) == 0 {
			return nil
		}
		sealedSources := sources
		sources = nil
		if err := m.sealAndCommit(ctx, writer, sealedSources, stats, policy); err != nil {
			return err
		}
		writer = nil
		return nil
	}
	for _, group := range mergeLogicalCandidates(candidates) {
		if err := ctx.Err(); err != nil {
			return err
		}
		note := func(skip looseSkip) {
			if policy.noteSkip != nil {
				policy.noteSkip(ctx, group, skip)
			}
		}
		// Readiness checks conflicting sizes, the size limit, and missing
		// files itself on every pass, so packing remembers only what it learns
		// by reading content.
		if group.err != nil {
			stats.BlobsCorrupt++
			note(looseUsable)
			continue
		}
		candidate := group.Candidate
		if candidate.Size > m.limits.BlobBytes {
			stats.BlobsDeferredOversized++
			note(looseUsable)
			continue
		}
		if writer != nil && len(sources) >= m.packedSourcePinLimit {
			if err := seal(); err != nil {
				return err
			}
		}
		prepared, source, sourcePin, found, readErr := m.prepareCandidate(ctx, candidate, policy.openPin)
		if readErr != nil {
			if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
				return readErr
			}
			// A failure that may pass on a retry, such as a full scratch
			// disk, must not hide the member from later readiness checks.
			_, retryable := errors.AsType[*retryableLooseError](readErr)
			skip := looseCorrupt
			if errors.Is(readErr, ErrBlobTooLarge) {
				stats.BlobsDeferredOversized++
				skip = looseOversized
			} else {
				stats.BlobsCorrupt++
			}
			if retryable {
				skip = looseUsable
			}
			note(skip)
			continue
		}
		if !found {
			stats.BlobsMissing++
			note(looseUsable)
			continue
		}
		if prepared == nil || sourcePin == nil {
			var closePreparedErr error
			if prepared != nil {
				closePreparedErr = prepared.Close()
			}
			return errors.Join(
				fmt.Errorf("packstore: prepared candidate %s has incomplete state", candidate.Hash),
				closePreparedErr,
				closeIdentityPin(sourcePin),
			)
		}
		if err := checkPlainOutput(m.limits, uint64(pack.MinEntryOffset), prepared.StoredLen(), 1); err != nil {
			if closeErr := errors.Join(prepared.Close(), sourcePin.Close()); closeErr != nil {
				return closeErr
			}
			stats.BlobsDeferredOversized++
			note(looseOversized)
			continue
		}
		note(looseUsable)
		if writer != nil {
			if err := checkPlainOutput(m.limits, uint64(writer.StoredSize()), prepared.StoredLen(), len(sources)+1); err != nil {
				if err := seal(); err != nil {
					return errors.Join(err, prepared.Close(), sourcePin.Close())
				}
			}
		}
		if writer == nil {
			newWriter, err := pack.NewWriter(m.layout.PacksDir(), pack.WriterOptions{TargetSize: opts.TargetSize})
			if err != nil {
				return errors.Join(err, prepared.Close(), sourcePin.Close())
			}
			writer = newWriter
		}
		entry, err := writer.AppendPrepared(ctx, prepared)
		if err != nil {
			return errors.Join(err, sourcePin.Close())
		}
		sources = append(sources, packedSource{candidate: candidate, path: source, pin: sourcePin, entry: entry})
		rawBytes += int64(entry.RawLen)
		if writer.Full() || (opts.MaxBytes > 0 && rawBytes >= opts.MaxBytes) {
			if err := seal(); err != nil {
				return err
			}
			if opts.MaxBytes > 0 && rawBytes >= opts.MaxBytes {
				stats.BudgetExhausted = true
				return nil
			}
		}
	}
	return seal()
}

func (m *Maintainer) prepareCandidate(
	ctx context.Context, candidate Candidate, openPin identityPinOpener,
) (*pack.PreparedBlob, string, identityPin, bool, error) {
	id, err := pack.ParseBlobID(candidate.Hash.String())
	if err != nil {
		return nil, "", nil, false, err
	}
	if candidate.Size < 0 {
		return nil, "", nil, false, fmt.Errorf("%w: negative loose logical size %d", ErrContentMismatch, candidate.Size)
	}
	if candidate.Size > m.limits.BlobBytes {
		return nil, "", nil, false, newLimitError(
			LimitBlobRawBytes, uint64(candidate.Size), uint64(m.limits.BlobBytes),
		)
	}
	var corrupt error
	var selected *pack.PreparedBlob
	var selectedPath string
	var selectedPin identityPin
	seenPaths := make(map[string]struct{}, len(candidate.Paths))
	for pathIndex, candidatePath := range candidate.Paths {
		if m.beforeCandidatePath != nil {
			m.beforeCandidatePath(pathIndex)
		}
		if err := ctx.Err(); err != nil {
			if selected != nil {
				err = errors.Join(err, selected.Close(), closeIdentityPin(selectedPin))
			}
			return nil, "", nil, false, err
		}
		path := resolveCandidatePath(m.layout.Root(), candidatePath)
		if _, duplicate := seenPaths[path]; duplicate {
			continue
		}
		seenPaths[path] = struct{}{}
		encoding := LooseEncodingRaw
		if canonicalLoosePathEqual(path, m.layout.CompressedLoosePath(candidate.Hash)) {
			encoding = LooseEncodingZstd
		}
		object, identity, err := openCandidateLooseObject(path, candidate.Hash, candidate.Size, encoding)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			corrupt = errors.Join(corrupt, classifyLooseFailure(err))
			continue
		}
		if object.storedSize > m.limits.BlobBytes {
			corrupt = errors.Join(
				corrupt,
				newLimitError(
					LimitBlobStoredBytes,
					uint64(object.storedSize),
					uint64(m.limits.BlobBytes),
				),
				object.file.Close(),
			)
			continue
		}
		var pin identityPin
		if selected == nil {
			pin, err = m.pinOpenCandidate(path, identity, openPin)
			if err != nil {
				corrupt = errors.Join(corrupt, classifyLooseFailure(err), object.file.Close())
				continue
			}
		}
		stream, err := newLooseVerifiedStream(ctx, candidate.Hash, object)
		if err != nil {
			corrupt = errors.Join(corrupt, classifyLooseFailure(err), closeIdentityPin(pin))
			continue
		}
		var prepared *pack.PreparedBlob
		var prepareErr error
		if selected == nil {
			prepared, prepareErr = pack.PrepareBlob(ctx, stream, uint64(candidate.Size), pack.DefaultZstdLevel, pack.AppendStreamOptions{
				ExpectedID: &id, ScratchDir: m.layout.PacksDir(),
			})
		}
		verifyErr := stream.Verify()
		closeErr := stream.Close()
		if err := errors.Join(prepareErr, verifyErr, closeErr); err != nil {
			if prepared != nil {
				err = errors.Join(err, prepared.Close())
			}
			err = errors.Join(err, closeIdentityPin(pin))
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				if selected != nil {
					err = errors.Join(err, selected.Close(), closeIdentityPin(selectedPin))
				}
				return nil, "", nil, false, err
			}
			corrupt = errors.Join(corrupt, classifyLooseFailure(err))
			continue
		}
		if selected == nil && prepared == nil {
			return nil, "", nil, false, errors.Join(
				fmt.Errorf("packstore: preparing candidate %s returned no result", candidate.Hash),
				closeIdentityPin(pin),
			)
		}
		if selected == nil {
			selected = prepared
			selectedPath = path
			selectedPin = pin
		}
	}
	if selected != nil {
		return selected, selectedPath, selectedPin, true, nil
	}
	return nil, "", nil, false, corrupt
}

// retryableLooseError marks a loose read failure that does not prove the
// content is bad, such as a permission error or a full scratch disk.
type retryableLooseError struct {
	err error
}

func (e *retryableLooseError) Error() string { return e.err.Error() }

func (e *retryableLooseError) Unwrap() error { return e.err }

// classifyLooseFailure marks err retryable unless reading showed that the
// content does not match its hash or size.
func classifyLooseFailure(err error) error {
	if errors.Is(err, ErrContentMismatch) || errors.Is(err, pack.ErrBlobMismatch) {
		return err
	}
	return &retryableLooseError{err: err}
}

func (m *Maintainer) pinOpenCandidate(
	path string, expected fs.FileInfo, openPin identityPinOpener,
) (identityPin, error) {
	pin, pinned, err := openPin(path)
	if err != nil {
		return nil, err
	}
	if pin == nil || pinned == nil {
		return nil, errors.Join(fmt.Errorf("packstore: identity pin for %s has incomplete state", path), closeIdentityPin(pin))
	}
	if !os.SameFile(expected, pinned) {
		return nil, errors.Join(errIdentityChanged, pin.Close())
	}
	return pin, nil
}

func closeIdentityPin(pin identityPin) error {
	if pin == nil {
		return nil
	}
	return pin.Close()
}

func closePackedSourcePins(sources []packedSource) error {
	var resultErr error
	for _, source := range sources {
		resultErr = errors.Join(resultErr, closeIdentityPin(source.pin))
	}
	return resultErr
}

func openCandidateLooseObject(
	path string,
	hash Hash,
	expectedSize int64,
	encoding LooseEncoding,
) (*looseObject, fs.FileInfo, error) {
	return openLooseObjectAtPath(path, hash, &expectedSize, encoding)
}

func openLooseObjectAtPath(
	path string,
	hash Hash,
	expectedSize *int64,
	encoding LooseEncoding,
) (*looseObject, fs.FileInfo, error) {
	identity, err := snapshotPathIdentity(path)
	if err != nil {
		return nil, nil, err
	}
	if err := validateRegularNoFollow(path, identity); err != nil {
		return nil, nil, err
	}
	f, info, err := openLooseFile(path)
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(identity, info) {
		return nil, nil, errors.Join(errIdentityChanged, f.Close())
	}
	logicalSize := info.Size()
	if encoding == LooseEncodingZstd {
		header := make([]byte, compressedLooseHeaderSize)
		if _, err := io.ReadFull(f, header); err != nil {
			return nil, nil, errors.Join(
				fmt.Errorf("%w: read compressed loose header: %w", ErrContentMismatch, err),
				f.Close(),
			)
		}
		logicalSize, err = decodeCompressedLooseHeader(header)
		if err != nil {
			return nil, nil, errors.Join(fmt.Errorf("%w: %w", ErrContentMismatch, err), f.Close())
		}
	}
	if expectedSize != nil && logicalSize != *expectedSize {
		return nil, nil, errors.Join(
			fmt.Errorf("%w: loose logical size is %d, want %d", ErrContentMismatch, logicalSize, *expectedSize),
			f.Close(),
		)
	}
	return &looseObject{
		file: f, encoding: encoding, logicalSize: logicalSize, storedSize: info.Size(),
	}, identity, nil
}

func verifyLoosePath(ctx context.Context, path string, hash Hash, limit int64) error {
	_, err := verifyLoosePathIdentity(ctx, path, hash, limit, LooseEncodingRaw)
	return err
}

func verifyLoosePathIdentity(
	ctx context.Context,
	path string,
	hash Hash,
	limit int64,
	encoding LooseEncoding,
) (fs.FileInfo, error) {
	pin, _, err := verifyLoosePathPinned(ctx, path, hash, limit, encoding, openLooseMaintenanceVerificationPin)
	if err != nil {
		return nil, err
	}
	identity, statErr := pin.Stat()
	return identity, errors.Join(statErr, pin.Close())
}

func verifyLoosePathPinned(
	ctx context.Context,
	path string,
	hash Hash,
	limit int64,
	encoding LooseEncoding,
	openPin identityPinOpener,
) (identityPin, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	object, identity, err := openLooseObjectAtPath(path, hash, nil, encoding)
	if err != nil {
		return nil, 0, err
	}
	size := object.logicalSize
	if size < 0 || size > limit {
		return nil, 0, errors.Join(
			newLimitError(LimitBlobRawBytes, uint64(size), uint64(limit)),
			object.file.Close(),
		)
	}
	if object.storedSize > limit {
		return nil, 0, errors.Join(
			newLimitError(LimitBlobStoredBytes, uint64(object.storedSize), uint64(limit)),
			object.file.Close(),
		)
	}
	pin, pinned, err := openPin(path)
	if err != nil {
		return nil, 0, errors.Join(err, object.file.Close())
	}
	if !os.SameFile(identity, pinned) {
		return nil, 0, errors.Join(errIdentityChanged, object.file.Close(), pin.Close())
	}
	stream, err := newLooseVerifiedStream(ctx, hash, object)
	if err != nil {
		return nil, 0, errors.Join(err, pin.Close())
	}
	if err := errors.Join(stream.Verify(), stream.Close()); err != nil {
		return nil, 0, errors.Join(err, pin.Close())
	}
	return pin, size, nil
}

func (m *Maintainer) findAnyValidCanonicalLoose(
	ctx context.Context, hash Hash,
) (int64, bool, error) {
	for _, candidate := range []struct {
		path     string
		encoding LooseEncoding
	}{
		{path: m.layout.CompressedLoosePath(hash), encoding: LooseEncodingZstd},
		{path: m.layout.LoosePath(hash), encoding: LooseEncodingRaw},
	} {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		pin, size, err := verifyLoosePathPinned(
			ctx,
			candidate.path,
			hash,
			m.limits.BlobBytes,
			candidate.encoding,
			openLooseMaintenanceVerificationPin,
		)
		if err == nil {
			if err := pin.Close(); err != nil {
				return 0, false, err
			}
			return size, true, nil
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 0, false, err
		}
	}
	return 0, false, nil
}

func (m *Maintainer) hasAuthoritativeCanonicalLoose(ctx context.Context, hash Hash) (bool, error) {
	for _, candidate := range []struct {
		path     string
		encoding LooseEncoding
	}{
		{path: m.layout.CompressedLoosePath(hash), encoding: LooseEncodingZstd},
		{path: m.layout.LoosePath(hash), encoding: LooseEncodingRaw},
	} {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		_, err := verifyLoosePathIdentity(ctx, candidate.path, hash, m.limits.BlobBytes, candidate.encoding)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, err
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
	}
	return false, nil
}

func verifyLooseFile(ctx context.Context, f *os.File, info fs.FileInfo, hash Hash) error {
	size := info.Size()
	digest := sha256.New()
	buffer := make([]byte, 64<<10)
	source := &contextio.Reader{Context: ctx, Reader: f}
	reader := io.LimitReader(source, size)
	written, err := io.CopyBuffer(digest, reader, buffer)
	if err != nil {
		return err
	}
	if written != size {
		return ErrContentMismatch
	}
	var probe [1]byte
	if n, err := source.Read(probe[:]); n != 0 || err == nil {
		return ErrContentMismatch
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		return errors.Join(err, errIdentityChanged)
	}
	if after.Size() != size {
		return ErrContentMismatch
	}
	if hex.EncodeToString(digest.Sum(nil)) != hash.String() {
		return ErrContentMismatch
	}
	return nil
}

func (m *Maintainer) sealAndCommit(
	ctx context.Context,
	writer *pack.Writer,
	sources []packedSource,
	stats *PackStats,
	policy packCandidatePolicy,
) (resultErr error) {
	defer func() {
		resultErr = errors.Join(resultErr, closePackedSourcePins(sources))
	}()
	packID := writer.ID()
	path := m.layout.PackPath(packID)
	entries, err := writer.Seal(path)
	if err != nil {
		return err
	}
	if err := validateSealedOutput(path, m.limits); err != nil {
		return err
	}
	if err := ValidateIndexEntries(indexEntriesFromPack(packID, entries)); err != nil {
		return err
	}
	record := PackRecord{PackID: packID, EntryCount: int64(len(entries)), CreatedAt: time.Now().UTC()}
	adoptions := make([]Adoption, len(sources))
	for i, source := range sources {
		entry := indexFromPack(packID, source.entry)
		record.StoredBytes += entry.StoredLen
		adoptions[i] = Adoption{Entry: entry, OriginalHashes: source.candidate.OriginalHashes}
	}
	if err := policy.commit(ctx, record, adoptions); err != nil {
		return err
	}
	stats.PacksSealed++
	stats.BlobsPacked += len(sources)
	if policy.replaceMappings {
		stats.MappingsPruned += int64(len(sources))
	}
	var cleanupErr error
	for index := range sources {
		source := sources[index]
		stats.BytesPacked += int64(source.entry.RawLen)
		if !policy.removeSource {
			continue
		}
		verifiedIdentity, statErr := source.pin.Stat()
		sources[index].pin = nil
		if statErr != nil {
			cleanupErr = errors.Join(cleanupErr, statErr, source.pin.Close())
			continue
		}
		removalPin, removalIdentity, openErr := m.openIdentityPin(source.path)
		closeErr := source.pin.Close()
		if closeErr != nil {
			cleanupErr = errors.Join(cleanupErr, closeErr, closeIdentityPin(removalPin))
			continue
		}
		if openErr != nil {
			if errors.Is(openErr, fs.ErrNotExist) || errors.Is(openErr, fs.ErrPermission) {
				continue
			}
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf(
				"packstore: open packed loose source %s for removal: %w", source.path, openErr,
			))
			continue
		}
		if !os.SameFile(verifiedIdentity, removalIdentity) {
			cleanupErr = errors.Join(cleanupErr, errIdentityChanged, removalPin.Close())
			continue
		}
		// Removal owns and closes the removal pin on every return path.
		if _, err := removeLoosePathPinned(source.path, removalPin); err != nil &&
			!errors.Is(err, errLooseRemovalUnavailable) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf(
				"packstore: remove packed loose source %s: %w", source.path, err,
			))
		}
	}
	return cleanupErr
}

func checkPlainOutput(limits Limits, dataEnd, nextStored uint64, entryCount int) error {
	if dataEnd > ^uint64(0)-nextStored {
		return newLimitError(LimitPackContainerBytes, ^uint64(0), uint64(limits.PackBytes))
	}
	containerBytes, footerBytes, err := pack.PlainPackSize(dataEnd+nextStored, entryCount)
	if err != nil {
		return err
	}
	if entryCount > limits.PackEntries {
		return newLimitError(LimitPackEntryCount, uint64(entryCount), uint64(limits.PackEntries))
	}
	if footerBytes > uint64(limits.FooterBytes) {
		return newLimitError(LimitPackFooterBytes, footerBytes, uint64(limits.FooterBytes))
	}
	if containerBytes > uint64(limits.PackBytes) {
		return newLimitError(LimitPackContainerBytes, containerBytes, uint64(limits.PackBytes))
	}
	return nil
}

func validateSealedOutput(path string, limits Limits) error {
	reader, err := OpenMaintenancePack(path, limits)
	if err != nil {
		return err
	}
	return reader.Close()
}

func (m *Maintainer) sweepLoose(ctx context.Context, refs map[Hash]Reference, allowOrphanRemoval bool, stats *PackStats) error {
	indexed, err := m.catalog.ListIndexed(ctx)
	if err != nil {
		return err
	}
	for _, entry := range indexed {
		hasCandidate, err := m.hasCanonicalLooseCandidate(ctx, entry.Hash)
		if err != nil {
			return err
		}
		if !hasCandidate {
			continue
		}
		if _, _, err := m.store.ReadBounded(ctx, entry.Hash, m.limits.BlobBytes); err != nil {
			continue
		}
		present := 0
		verified := 0
		for _, candidate := range []struct {
			path     string
			encoding LooseEncoding
		}{
			{path: m.layout.LoosePath(entry.Hash), encoding: LooseEncodingRaw},
			{path: m.layout.CompressedLoosePath(entry.Hash), encoding: LooseEncodingZstd},
		} {
			physical, err := snapshotPathIdentity(candidate.path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			} else if err != nil {
				return err
			}
			if err := validateRegularNoFollow(candidate.path, physical); err != nil {
				continue
			}
			present++
			verificationPin, _, err := verifyLoosePathPinned(
				ctx,
				candidate.path,
				entry.Hash,
				m.limits.BlobBytes,
				candidate.encoding,
				openLooseMaintenanceVerificationPin,
			)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				continue
			}
			verified++
			verifiedIdentity, statErr := verificationPin.Stat()
			removalPin, removalIdentity, openErr := m.openIdentityPin(candidate.path)
			if openErr != nil {
				if closeErr := verificationPin.Close(); closeErr != nil {
					return closeErr
				}
				continue
			}
			closeErr := verificationPin.Close()
			if err := errors.Join(statErr, closeErr); err != nil {
				return errors.Join(err, removalPin.Close())
			}
			if !os.SameFile(verifiedIdentity, removalIdentity) {
				if err := removalPin.Close(); err != nil {
					return err
				}
				continue
			}
			removed, removeErr := removeLoosePathPinned(candidate.path, removalPin)
			if errors.Is(removeErr, errLooseRemovalUnavailable) {
				continue
			}
			if removeErr != nil {
				return fmt.Errorf("packstore: sweep loose content: %w", removeErr)
			}
			if removed {
				stats.LooseSwept++
			}
		}
		if present > 0 && verified == 0 {
			stats.BlobsCorrupt++
		}
	}
	if !allowOrphanRemoval {
		stats.LooseOrphanSweepSuppressed = true
		return nil
	}
	stats.LooseOrphansRemoved, stats.LooseOrphansDeferred, err = m.removeLooseOrphans(ctx, refs)
	return err
}

// removeLooseOrphans removes canonical loose files absent from a complete reference
// inventory and counts those it removed and those it lacked permission to remove.
// A missing root holds no files. The caller holds the maintenance lease.
func (m *Maintainer) removeLooseOrphans(ctx context.Context, refs map[Hash]Reference) (removed, deferred int, err error) {
	entries, err := os.ReadDir(m.layout.Root())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	for _, shard := range entries {
		if err := ctx.Err(); err != nil {
			return removed, deferred, err
		}
		if !shard.IsDir() || len(shard.Name()) != 2 || shard.Name() == "pa" {
			continue
		}
		files, err := os.ReadDir(filepath.Join(m.layout.Root(), shard.Name()))
		if err != nil {
			return removed, deferred, err
		}
		for _, file := range files {
			if err := ctx.Err(); err != nil {
				return removed, deferred, err
			}
			hash, encoding, ok := parseCanonicalLooseName(file.Name())
			if !ok || hash.String()[:2] != shard.Name() || !file.Type().IsRegular() {
				continue
			}
			if _, live := refs[hash]; live {
				continue
			}
			path := m.layout.LoosePath(hash)
			if encoding == LooseEncodingZstd {
				path = m.layout.CompressedLoosePath(hash)
			}
			if filepath.Clean(path) != filepath.Clean(filepath.Join(m.layout.Root(), shard.Name(), file.Name())) {
				continue
			}
			// One file without removal permission must not block reclaiming the rest.
			// Permission is classified only at pin acquisition: later permission
			// failures that left cleanup undone stay fatal.
			pin, _, err := m.openIdentityPin(path)
			if errors.Is(err, fs.ErrPermission) {
				deferred++
				continue
			}
			var fileRemoved bool
			if err == nil {
				fileRemoved, err = removeLoosePathPinned(path, pin)
			}
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if errors.Is(err, ErrContentMismatch) {
				continue
			}
			if errors.Is(err, errLooseRemovalUnavailable) {
				deferred++
				continue
			}
			if err != nil {
				return removed, deferred, err
			}
			if fileRemoved {
				removed++
			}
		}
	}
	return removed, deferred, nil
}

// OrphanSweepStats summarizes an orphan sweep.
type OrphanSweepStats struct {
	LooseOrphansRemoved int
	// LooseOrphansDeferred counts orphans left in place because the store
	// lacked permission to remove them. Later sweeps retry them.
	LooseOrphansDeferred int
	// Suppressed reports an incomplete reference inventory, so nothing was removed.
	Suppressed bool
}

// SweepOrphans removes canonical loose files the catalog does not reference,
// without verifying, packing or repacking. Like Pack it holds the maintenance
// lease, so it waits for in-flight writes, and it removes nothing when the
// reference inventory is incomplete. A caller with a short time budget can
// run it before Pack so unreferenced space is reclaimed even when Pack does
// not finish.
func (m *Maintainer) SweepOrphans(ctx context.Context) (OrphanSweepStats, error) {
	var stats OrphanSweepStats
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	lease, err := m.coordinator.AcquireMaintenance(ctx)
	if err != nil {
		return stats, err
	}
	defer func() { _ = lease.Release() }()
	inventory, err := m.catalog.ListReferences(ctx)
	if err != nil {
		return stats, err
	}
	if !inventory.Complete {
		stats.Suppressed = true
		return stats, nil
	}
	refs := make(map[Hash]Reference, len(inventory.References))
	for _, reference := range inventory.References {
		refs[reference.Hash] = reference
	}
	stats.LooseOrphansRemoved, stats.LooseOrphansDeferred, err = m.removeLooseOrphans(ctx, refs)
	return stats, err
}

// hasCanonicalLooseCandidate is an authorization-free, no-follow preflight.
// A negative result only defers work; deletion still requires a second open,
// full verification, and a live identity pin after packed authority verifies.
func (m *Maintainer) hasCanonicalLooseCandidate(ctx context.Context, hash Hash) (bool, error) {
	for _, path := range []string{m.layout.LoosePath(hash), m.layout.CompressedLoosePath(hash)} {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		identity, err := snapshotPathIdentity(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if err := validateRegularNoFollow(path, identity); err == nil {
			return true, nil
		}
	}
	return false, nil
}

func parseCanonicalLooseName(name string) (Hash, LooseEncoding, bool) {
	encoding := LooseEncodingRaw
	hashName := name
	if strings.HasSuffix(name, ".zst") {
		encoding = LooseEncodingZstd
		hashName = strings.TrimSuffix(name, ".zst")
	}
	hash, err := ParseHash(hashName)
	return hash, encoding, err == nil
}

// removeLoosePathPinned moves the current directory entry into an exclusive
// sibling aside before deciding whether to delete it. The move closes the
// validation-to-unlink race at path: a replacement is restored without
// clobbering any still-newer occupant, while only the expected identity is
// discarded.
func removeLoosePath(path string, openPin identityPinOpener) (removed bool, resultErr error) {
	pin, _, err := openPin(path)
	if err != nil {
		return false, err
	}
	return removeLoosePathPinned(path, pin)
}

func removeLoosePathPinned(path string, pin identityPin) (bool, error) {
	return removeLoosePathPinnedWithOwnership(path, pin, true)
}

func removeLoosePathPinnedWithOwnership(
	path string,
	pin identityPin,
	consumePin bool,
) (removed bool, resultErr error) {
	pinOpen := true
	defer func() {
		if consumePin && pinOpen {
			resultErr = errors.Join(resultErr, pin.Close())
		}
	}()
	identity, err := pin.Stat()
	if err != nil {
		return false, fmt.Errorf("inspect pinned loose content %s: %w", path, err)
	}
	removalUnavailable := func(cause error) (bool, error) {
		current, currentErr := snapshotPathIdentity(path)
		if currentErr != nil {
			return false, errors.Join(cause, currentErr)
		}
		if validationErr := validateRegularNoFollow(path, current); validationErr != nil {
			return false, errors.Join(cause, validationErr)
		}
		if !os.SameFile(identity, current) {
			return false, errors.Join(cause, errIdentityChanged)
		}
		if consumePin {
			closeErr := pin.Close()
			pinOpen = false
			if closeErr != nil {
				return false, errors.Join(cause, closeErr)
			}
		}
		return false, errors.Join(errLooseRemovalUnavailable, cause)
	}
	asideDir, err := makeLooseRemovalAside(path)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return removalUnavailable(err)
		}
		return false, err
	}
	aside := filepath.Join(asideDir, "claimed")
	beforeLooseRemovalClaim(path)
	if err := claimLooseRemovalPath(path, aside); err != nil {
		cleanupErr := removeLooseRemovalAside(asideDir)
		if errors.Is(err, fs.ErrNotExist) {
			return false, cleanupErr
		}
		if errors.Is(err, fs.ErrPermission) && cleanupErr == nil {
			return removalUnavailable(fmt.Errorf("claim %s for removal: %w", path, err))
		}
		return false, errors.Join(
			fmt.Errorf("claim %s for removal: %w", path, err),
			cleanupErr,
		)
	}
	claimed, err := snapshotPathIdentity(aside)
	if err != nil {
		return false, fmt.Errorf(
			"inspect claimed loose content %s (preserved at %s): %w", path, aside, err,
		)
	}
	if !os.SameFile(identity, claimed) {
		restoreErr := restoreLooseRemovalClaim(path, aside, claimed)
		if restoreErr != nil {
			return false, errors.Join(errIdentityChanged, restoreErr)
		}
		return false, errIdentityChanged
	}
	if err := validateRegularNoFollow(aside, claimed); err != nil {
		return false, fmt.Errorf(
			"validate claimed loose content %s (preserved at %s): %w", path, aside, err,
		)
	}
	if err := removePinnedLooseClaim(pin, aside); err != nil {
		removeErr := fmt.Errorf("remove claimed loose content %s: %w", aside, err)
		restoreErr := restoreLooseRemovalClaim(path, aside, claimed)
		if errors.Is(err, fs.ErrPermission) && restoreErr == nil {
			return removalUnavailable(removeErr)
		}
		return false, errors.Join(removeErr, restoreErr)
	}
	if consumePin {
		closeErr := pin.Close()
		pinOpen = false
		if closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close removed loose content pin %s: %w", aside, closeErr))
		}
	}
	if err := removeLooseRemovalAside(asideDir); err != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("remove empty loose removal aside %s: %w", asideDir, err))
	}
	return true, resultErr
}

// makeLooseRemovalAside uses an exclusive directory rather than renaming
// directly to a random sibling file. atomicfile.Replace replaces existing
// destinations; the private directory makes the final claim name known-absent.
func makeLooseRemovalAside(path string) (string, error) {
	const attempts = 8
	for range attempts {
		aside := filepath.Join(
			filepath.Dir(path),
			"."+filepath.Base(path)+".remove-"+pack.NewPackID(),
		)
		if err := createLooseRemovalAside(aside); err == nil {
			return aside, nil
		} else if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("create loose removal aside for %s: %w", path, err)
		}
	}
	return "", fmt.Errorf("create unique loose removal aside for %s: %w", path, fs.ErrExist)
}

// restoreLooseRemovalClaim puts a foreign claimed entry back without replacing
// a newer entry at path. Failures preserve the claim at the path named in the
// returned error for diagnosis and manual recovery.
func restoreLooseRemovalClaim(path, aside string, claimed fs.FileInfo) error {
	current, err := snapshotPathIdentity(aside)
	if err != nil {
		return fmt.Errorf("inspect foreign loose removal claim %s: %w", aside, err)
	}
	if !os.SameFile(claimed, current) {
		return fmt.Errorf("%w: foreign loose removal claim %s changed before restore", errIdentityChanged, aside)
	}
	switch {
	case claimed.Mode().IsRegular():
		if err := linkLooseRemovalClaim(path, aside, claimed); err != nil {
			return err
		}
	case claimed.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(aside)
		if err != nil {
			return fmt.Errorf("read foreign loose symlink %s for restore: %w", aside, err)
		}
		if err := os.Symlink(target, path); err != nil {
			return fmt.Errorf("restore foreign loose symlink from %s to %s without clobbering: %w", aside, path, err)
		}
		// Symlink creates with no replacement are atomic. Once this succeeds,
		// any subsequent removal or replacement at path is a later operation.
		afterLooseRemovalRestorePublish(path)
	default:
		return fmt.Errorf("foreign loose content at %s has unsupported mode %s; preserved at %s", path, claimed.Mode(), aside)
	}
	current, err = snapshotPathIdentity(aside)
	if err != nil {
		return fmt.Errorf("recheck restored foreign loose claim %s: %w", aside, err)
	}
	if !os.SameFile(claimed, current) {
		return fmt.Errorf("%w: restored foreign loose claim %s changed before cleanup", errIdentityChanged, aside)
	}
	if err := removeLooseCanonicalFile(aside); err != nil {
		return fmt.Errorf("clean restored foreign loose claim %s: %w", aside, err)
	}
	if err := removeLooseRemovalAside(filepath.Dir(aside)); err != nil {
		return fmt.Errorf("remove restored loose removal aside %s: %w", filepath.Dir(aside), err)
	}
	return nil
}

// linkLooseRemovalClaim restores the exact claimed inode with a no-clobber hard
// link. The namespace identity pin does not require content-read access.
// Writers that already hold the inode therefore remain attached to the restored
// name, and no copied snapshot can overwrite their later changes.
func linkLooseRemovalClaim(path, aside string, claimed fs.FileInfo) (resultErr error) {
	source, sourceIdentity, err := openLooseRestorationIdentityPin(aside)
	if err != nil {
		return fmt.Errorf("pin foreign loose content %s for restore: %w", aside, err)
	}
	defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
	if !os.SameFile(claimed, sourceIdentity) {
		return errors.Join(errIdentityChanged, fmt.Errorf("foreign loose removal claim %s changed while opening", aside))
	}
	beforeLooseRemovalRestorePublish(aside, path)
	if err := publishLooseRemovalRestoreFile(aside, path); err != nil {
		return fmt.Errorf("publish restored foreign loose content from %s to %s without clobbering: %w", aside, path, err)
	}
	pinned, pinErr := source.Stat()
	canonical, canonicalErr := snapshotPathIdentity(path)
	current, currentErr := snapshotPathIdentity(aside)
	if err := errors.Join(pinErr, canonicalErr, currentErr); err != nil {
		return fmt.Errorf("recheck linked foreign loose claim %s: %w", aside, err)
	}
	if !os.SameFile(claimed, pinned) ||
		!os.SameFile(pinned, canonical) ||
		!os.SameFile(pinned, current) {
		return fmt.Errorf("%w: linked foreign loose claim %s changed identity", errIdentityChanged, aside)
	}
	// The exact claimed inode is now atomically visible at path. Later writes,
	// removal, or replacement are later actions and cannot make claim cleanup
	// discard a stale copy.
	afterLooseRemovalRestorePublish(path)
	return nil
}

// canonicalLoosePathEqual is lexical only: Windows folds case, while no
// platform resolves symlink aliases or broadens the canonical namespace.
var canonicalLoosePathEqual = func(left, right string) bool {
	return canonicalLoosePathEqualForOS(runtime.GOOS, left, right)
}

func canonicalLoosePathEqualForOS(goos, left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if goos == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func indexFromPack(packID string, entry pack.Entry) IndexEntry {
	hash, _ := ParseHash(entry.ID.String())
	return IndexEntry{
		Hash: hash, PackID: packID, Offset: int64(entry.Offset),
		StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen),
		Flags: uint8(entry.Flags), CRC32C: entry.CRC32C,
	}
}

func indexEntriesFromPack(packID string, entries []pack.Entry) []IndexEntry {
	result := make([]IndexEntry, len(entries))
	for i, entry := range entries {
		result[i] = indexFromPack(packID, entry)
	}
	return result
}
