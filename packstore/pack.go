package packstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go.kenn.io/kit/pack"
)

// PackOptions bounds one packing run. MaxBytes is a soft committed raw-byte
// budget; zero is unlimited.
type PackOptions struct {
	TargetSize int64
	MaxBytes   int64
}

// PackStats summarizes committed and repair outcomes.
type PackStats struct {
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
	LooseOrphanSweepSuppressed bool
	BudgetExhausted            bool
}

// Pack repairs inventory, reconciles orphan packs, packs loose members, and
// sweeps verified redundant loose files in that order.
func (m *Maintainer) Pack(ctx context.Context, opts PackOptions) (PackStats, error) {
	var stats PackStats
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	if opts.TargetSize < 0 || opts.MaxBytes < 0 {
		return stats, ErrInvalidPolicy
	}
	lease, err := m.coordinator.AcquireMaintenance(ctx)
	if err != nil {
		return stats, err
	}
	defer func() { _ = lease.Release() }()
	if err := pack.MkdirAllSynced(m.layout.PacksDir()); err != nil {
		return stats, err
	}
	if err := m.cleanPackStaging(ctx); err != nil {
		return stats, err
	}
	if err := m.dropDangling(ctx, &stats); err != nil {
		return stats, err
	}
	pruned, err := m.catalog.PruneUnreferenced(ctx)
	if err != nil {
		return stats, err
	}
	stats.MappingsPruned += pruned
	inventory, err := m.catalog.ListReferences(ctx)
	if err != nil {
		return stats, err
	}
	refs := make(map[Hash]Reference, len(inventory.References))
	for _, reference := range inventory.References {
		refs[reference.Hash] = reference
	}
	if inventory.Complete {
		if err := m.reconcileOrphans(ctx, refs, &stats); err != nil {
			return stats, err
		}
	}
	if err := m.packLoose(ctx, opts, &stats); err != nil {
		return stats, err
	}
	if err := m.sweepLoose(ctx, refs, inventory.Complete, &stats); err != nil {
		return stats, err
	}
	return stats, nil
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

func (m *Maintainer) dropDangling(ctx context.Context, stats *PackStats) error {
	records, err := m.catalog.ListPackRecords(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := os.Stat(m.layout.PackPath(record.PackID)); errors.Is(err, fs.ErrNotExist) {
			if err := m.catalog.DeletePackRecord(ctx, record.PackID); err != nil {
				return err
			}
			stats.RecordsDropped++
		} else if err != nil {
			return err
		}
	}
	indexed, err := m.catalog.ListIndexed(ctx)
	if err != nil {
		return err
	}
	for _, entry := range indexed {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := os.Stat(m.layout.PackPath(entry.PackID)); errors.Is(err, fs.ErrNotExist) {
			if err := m.catalog.DeleteIndexEntry(ctx, entry.Hash); err != nil {
				return err
			}
			stats.MappingsPruned++
		} else if err != nil {
			return err
		} else {
			if _, _, err := m.store.readPackedBounded(ctx, entry.Hash, &entry, m.limits.BlobBytes); err == nil {
				continue
			}
			valid, err := m.hasValidCanonicalLoose(ctx, entry.Hash)
			if err != nil {
				return err
			}
			if !valid {
				continue
			}
			if err := m.catalog.DeleteIndexEntry(ctx, entry.Hash); err != nil {
				return err
			}
			stats.MappingsPruned++
		}
	}
	return nil
}

func (m *Maintainer) reconcileOrphans(ctx context.Context, refs map[Hash]Reference, stats *PackStats) error {
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
		hasRecord, err := m.catalog.HasPackRecord(ctx, packID)
		if err != nil || hasRecord {
			return err
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
		record.StoredBytes += int64(footerEntry.StoredLen) //nolint:gosec
		hash, err := ParseHash(footerEntry.ID.String())
		if err != nil {
			return err
		}
		reference, live := refs[hash]
		if !live {
			continue
		}
		if footerEntry.RawLen > uint64(m.limits.BlobBytes) || footerEntry.StoredLen > uint64(m.limits.BlobBytes) { //nolint:gosec
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
			valid, verifyErr := m.hasValidCanonicalLoose(ctx, hash)
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
	identity  fs.FileInfo
	entry     pack.Entry
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
			groups = append(groups, candidateGroup{Candidate: Candidate{
				Hash: candidate.Hash,
				Size: candidate.Size,
			}})
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

func (m *Maintainer) packLoose(ctx context.Context, opts PackOptions, stats *PackStats) error {
	candidates, err := m.catalog.ListUnpacked(ctx)
	if err != nil {
		return err
	}
	var writer *pack.Writer
	var sources []packedSource
	var rawBytes int64
	abort := func() {
		if writer != nil {
			_ = writer.Abort()
		}
	}
	defer abort()
	seal := func() error {
		if writer == nil || len(sources) == 0 {
			return nil
		}
		if err := m.sealAndRecord(ctx, writer, sources, stats); err != nil {
			return err
		}
		writer = nil
		sources = nil
		return nil
	}
	for _, group := range mergeLogicalCandidates(candidates) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if group.err != nil {
			stats.BlobsCorrupt++
			continue
		}
		candidate := group.Candidate
		if candidate.Size > m.limits.BlobBytes {
			stats.BlobsDeferredOversized++
			continue
		}
		prepared, source, sourceIdentity, found, readErr := m.prepareCandidate(ctx, candidate)
		if readErr != nil {
			if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
				return readErr
			}
			if errors.Is(readErr, ErrBlobTooLarge) {
				stats.BlobsDeferredOversized++
				continue
			}
			stats.BlobsCorrupt++
			continue
		}
		if !found {
			stats.BlobsMissing++
			continue
		}
		if err := checkPlainOutput(m.limits, uint64(pack.MinEntryOffset), prepared.StoredLen(), 1); err != nil {
			_ = prepared.Close()
			stats.BlobsDeferredOversized++
			continue
		}
		if writer != nil {
			if err := checkPlainOutput(m.limits, uint64(writer.StoredSize()), prepared.StoredLen(), len(sources)+1); err != nil { //nolint:gosec // writer offsets are non-negative
				if err := seal(); err != nil {
					_ = prepared.Close()
					return err
				}
			}
		}
		if writer == nil {
			writer, err = pack.NewWriter(m.layout.PacksDir(), pack.WriterOptions{TargetSize: opts.TargetSize})
			if err != nil {
				_ = prepared.Close()
				return err
			}
		}
		entry, err := writer.AppendPrepared(ctx, prepared)
		if err != nil {
			return err
		}
		sources = append(sources, packedSource{candidate: candidate, path: source, identity: sourceIdentity, entry: entry})
		rawBytes += int64(entry.RawLen) //nolint:gosec // bounded by configured limits
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
	ctx context.Context, candidate Candidate,
) (*pack.PreparedBlob, string, fs.FileInfo, bool, error) {
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
	var selectedIdentity fs.FileInfo
	seenPaths := make(map[string]struct{}, len(candidate.Paths))
	for _, candidatePath := range candidate.Paths {
		if err := ctx.Err(); err != nil {
			if selected != nil {
				_ = selected.Close()
			}
			return nil, "", nil, false, err
		}
		path := candidatePath
		if !filepath.IsAbs(path) {
			path = filepath.Join(m.layout.Root(), filepath.FromSlash(path))
		}
		path = filepath.Clean(path)
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
			corrupt = errors.Join(corrupt, err)
			continue
		}
		stream, err := newLooseVerifiedStream(ctx, candidate.Hash, object)
		if err != nil {
			corrupt = errors.Join(corrupt, err)
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
				_ = prepared.Close()
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				if selected != nil {
					_ = selected.Close()
				}
				return nil, "", nil, false, err
			}
			corrupt = errors.Join(corrupt, err)
			continue
		}
		if selected == nil {
			selected = prepared
			selectedPath = path
			selectedIdentity = identity
		}
	}
	if selected != nil {
		return selected, selectedPath, selectedIdentity, true, nil
	}
	return nil, "", nil, false, corrupt
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
				fmt.Errorf("%w: read compressed loose header: %v", ErrContentMismatch, err),
				f.Close(),
			)
		}
		logicalSize, err = decodeCompressedLooseHeader(header)
		if err != nil {
			return nil, nil, errors.Join(fmt.Errorf("%w: %v", ErrContentMismatch, err), f.Close())
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	object, identity, err := openLooseObjectAtPath(path, hash, nil, encoding)
	if err != nil {
		return nil, err
	}
	size := object.logicalSize
	if size < 0 || size > limit {
		return nil, errors.Join(
			newLimitError(LimitBlobRawBytes, uint64(size), uint64(limit)), //nolint:gosec
			object.file.Close(),
		)
	}
	stream, err := newLooseVerifiedStream(ctx, hash, object)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(stream.Verify(), stream.Close()); err != nil {
		return nil, err
	}
	return identity, nil
}

func (m *Maintainer) hasValidCanonicalLoose(ctx context.Context, hash Hash) (bool, error) {
	valid := false
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
		if _, err := verifyLoosePathIdentity(ctx, candidate.path, hash, m.limits.BlobBytes, candidate.encoding); err == nil {
			valid = true
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, err
		}
	}
	return valid, nil
}

func verifyLooseFile(ctx context.Context, f *os.File, info fs.FileInfo, hash Hash) error {
	size := info.Size()
	digest := sha256.New()
	buffer := make([]byte, 64<<10)
	source := &contextReader{ctx: ctx, reader: f}
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

func (m *Maintainer) sealAndRecord(ctx context.Context, writer *pack.Writer, sources []packedSource, stats *PackStats) error {
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
	if err := m.catalog.RecordPack(ctx, record, adoptions); err != nil {
		return err
	}
	stats.PacksSealed++
	stats.BlobsPacked += len(sources)
	var cleanupErr error
	for _, source := range sources {
		stats.BytesPacked += int64(source.entry.RawLen) //nolint:gosec
		if _, err := removeLoosePathIdentity(source.path, source.identity); err != nil {
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
			identity, err := verifyLoosePathIdentity(
				ctx, candidate.path, entry.Hash, m.limits.BlobBytes, candidate.encoding,
			)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				continue
			}
			verified++
			removed, removeErr := removeLoosePathIdentity(candidate.path, identity)
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
	entries, err := os.ReadDir(m.layout.Root())
	if err != nil {
		return err
	}
	if !allowOrphanRemoval {
		stats.LooseOrphanSweepSuppressed = true
		return nil
	}
	for _, shard := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !shard.IsDir() || len(shard.Name()) != 2 || shard.Name() == "pa" {
			continue
		}
		files, err := os.ReadDir(filepath.Join(m.layout.Root(), shard.Name()))
		if err != nil {
			return err
		}
		for _, file := range files {
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
			identity, err := snapshotPathIdentity(path)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return err
			}
			if err := validateRegularNoFollow(path, identity); err != nil {
				continue
			}
			removed, err := removeLoosePathIdentity(path, identity)
			if err != nil {
				return err
			}
			if removed {
				stats.LooseOrphansRemoved++
			}
		}
	}
	return nil
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

// removeLoosePathIdentity moves the current directory entry into an exclusive
// sibling aside before deciding whether to delete it. The move closes the
// validation-to-unlink race at path: a replacement is restored without
// clobbering any still-newer occupant, while only the expected identity is
// discarded.
func removeLoosePathIdentity(path string, identity fs.FileInfo) (bool, error) {
	asideDir, err := makeLooseRemovalAside(path)
	if err != nil {
		return false, err
	}
	aside := filepath.Join(asideDir, "claimed")
	beforeLooseRemovalClaim(path)
	if err := claimLooseRemovalPath(path, aside); err != nil {
		cleanupErr := removeLooseRemovalAside(asideDir)
		if errors.Is(err, fs.ErrNotExist) {
			return false, cleanupErr
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
	if err := removeLooseCanonicalFile(aside); err != nil {
		removeErr := fmt.Errorf("remove claimed loose content %s: %w", aside, err)
		return false, errors.Join(removeErr, restoreLooseRemovalClaim(path, aside, claimed))
	}
	if err := removeLooseRemovalAside(asideDir); err != nil {
		return true, fmt.Errorf("remove empty loose removal aside %s: %w", asideDir, err)
	}
	return true, nil
}

// makeLooseRemovalAside uses an exclusive directory rather than renaming
// directly to a random sibling file. os.Rename replaces existing destinations
// on Unix; the private directory makes the final claim name known-absent.
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
		if err := copyLooseRemovalClaim(path, aside, claimed); err != nil {
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

// copyLooseRemovalClaim restores regular content through a pinned no-follow
// descriptor into private staging. Only after the complete staging file is
// synced does a no-clobber hard link publish it at path. Successful publication
// is the linearization point: later removal or replacement at path is a later
// operation and does not make cleanup of the original claim unsafe.
func copyLooseRemovalClaim(path, aside string, claimed fs.FileInfo) error {
	source, sourceIdentity, err := openLooseFile(aside)
	if err != nil {
		return fmt.Errorf("open foreign loose content %s for restore: %w", aside, err)
	}
	if !os.SameFile(claimed, sourceIdentity) {
		return errors.Join(
			errIdentityChanged,
			fmt.Errorf("foreign loose removal claim %s changed while opening", aside),
			source.Close(),
		)
	}
	stagingPath := filepath.Join(filepath.Dir(aside), "restore")
	destination, err := createLooseRemovalRestoreFile(stagingPath, claimed.Mode().Perm())
	if err != nil {
		return errors.Join(
			fmt.Errorf("stage foreign loose content from %s for restore: %w", aside, err),
			source.Close(),
		)
	}
	destinationIdentity, statErr := destination.Stat()
	buffer := looseCopyBufferPool.Get().(*[looseCopyBufferBytes]byte)
	_, copyErr := io.CopyBuffer(destination, source, buffer[:])
	looseCopyBufferPool.Put(buffer)
	after, afterErr := source.Stat()
	syncErr := destination.Sync()
	closeErr := destination.Close()
	sourceCloseErr := source.Close()
	if afterErr == nil && !os.SameFile(claimed, after) {
		afterErr = errIdentityChanged
	}
	if restoreErr := errors.Join(statErr, copyErr, afterErr, syncErr, closeErr, sourceCloseErr); restoreErr != nil {
		cleanupErr := removeLooseRemovalRestoreStaging(stagingPath, destinationIdentity)
		return errors.Join(
			fmt.Errorf("copy foreign loose content from %s into private staging: %w", aside, restoreErr),
			cleanupErr,
		)
	}
	beforeLooseRemovalRestorePublish(stagingPath, path)
	if err := publishLooseRemovalRestoreFile(stagingPath, path); err != nil {
		return errors.Join(
			fmt.Errorf("publish restored foreign loose content from %s to %s without clobbering: %w", stagingPath, path, err),
			removeLooseRemovalRestoreStaging(stagingPath, destinationIdentity),
		)
	}
	// The complete, synced inode is now atomically visible at path. Cleanup no
	// longer depends on path retaining that entry.
	afterLooseRemovalRestorePublish(path)
	if err := removeLooseRemovalRestoreStaging(stagingPath, destinationIdentity); err != nil {
		return fmt.Errorf("clean published loose restore staging %s: %w", stagingPath, err)
	}
	return nil
}

func removeLooseRemovalRestoreStaging(path string, identity fs.FileInfo) error {
	if identity == nil {
		return nil
	}
	current, err := snapshotPathIdentity(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(identity, current) {
		return fmt.Errorf("%w: loose removal restore staging %s changed identity", errIdentityChanged, path)
	}
	return removeLooseCanonicalFile(path)
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
	return IndexEntry{Hash: hash, PackID: packID, Offset: int64(entry.Offset),
		StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen),
		Flags: uint8(entry.Flags), CRC32C: entry.CRC32C}
}

func indexEntriesFromPack(packID string, entries []pack.Entry) []IndexEntry {
	result := make([]IndexEntry, len(entries))
	for i, entry := range entries {
		result[i] = indexFromPack(packID, entry)
	}
	return result
}
