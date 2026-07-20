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

func (m *Maintainer) packLoose(ctx context.Context, opts PackOptions, stats *PackStats) error {
	candidates, err := m.catalog.ListUnpacked(ctx)
	if err != nil {
		return err
	}
	seen := make(map[Hash]struct{}, len(candidates))
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
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, duplicate := seen[candidate.Hash]; duplicate {
			continue
		}
		seen[candidate.Hash] = struct{}{}
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
		object, identity, err := openCandidateLooseObject(path, candidate.Hash, candidate.Size)
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

func openCandidateLooseObject(path string, hash Hash, expectedSize int64) (*looseObject, fs.FileInfo, error) {
	return openLooseObjectAtPath(path, hash, &expectedSize)
}

func openLooseObjectAtPath(path string, hash Hash, expectedSize *int64) (*looseObject, fs.FileInfo, error) {
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
	encoding := LooseEncodingRaw
	logicalSize := info.Size()
	if filepath.Base(path) == hash.String()+".zst" {
		encoding = LooseEncodingZstd
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
	_, err := verifyLoosePathIdentity(ctx, path, hash, limit)
	return err
}

func verifyLoosePathIdentity(ctx context.Context, path string, hash Hash, limit int64) (fs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	object, identity, err := openLooseObjectAtPath(path, hash, nil)
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
	for _, path := range []string{m.layout.CompressedLoosePath(hash), m.layout.LoosePath(hash)} {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := verifyLoosePath(ctx, path, hash, m.limits.BlobBytes); err == nil {
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
	for _, source := range sources {
		stats.BytesPacked += int64(source.entry.RawLen) //nolint:gosec
		if current, err := snapshotPathIdentity(source.path); err == nil && os.SameFile(source.identity, current) {
			_ = os.Remove(source.path)
		}
	}
	return nil
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
		for _, path := range []string{m.layout.LoosePath(entry.Hash), m.layout.CompressedLoosePath(entry.Hash)} {
			physical, err := snapshotPathIdentity(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			} else if err != nil {
				return err
			}
			if err := validateRegularNoFollow(path, physical); err != nil {
				continue
			}
			present++
			identity, err := verifyLoosePathIdentity(ctx, path, entry.Hash, m.limits.BlobBytes)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				continue
			}
			verified++
			removed, _ := removeLoosePathIdentity(path, identity)
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

func removeLoosePathIdentity(path string, identity fs.FileInfo) (bool, error) {
	current, err := snapshotPathIdentity(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := validateRegularNoFollow(path, current); err != nil {
		return false, err
	}
	if !os.SameFile(identity, current) {
		return false, errIdentityChanged
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
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
