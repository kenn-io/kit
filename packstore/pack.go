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
	PacksSealed            int
	BlobsPacked            int
	BytesPacked            int64
	PacksAdopted           int
	PacksRemoved           int
	PacksQuarantined       int
	PacksUnreadable        int
	RecordsDropped         int
	MappingsPruned         int64
	BlobsMissing           int
	BlobsCorrupt           int
	BlobsDeferredOversized int
	PacksDeferredOversized int
	LooseSwept             int
	LooseOrphansRemoved    int
	BudgetExhausted        bool
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
	references, err := m.catalog.ListReferences(ctx)
	if err != nil {
		return stats, err
	}
	refs := make(map[Hash]Reference, len(references))
	for _, reference := range references {
		refs[reference.Hash] = reference
	}
	if err := m.reconcileOrphans(ctx, refs, &stats); err != nil {
		return stats, err
	}
	if err := m.packLoose(ctx, opts, &stats); err != nil {
		return stats, err
	}
	if err := m.sweepLoose(ctx, refs, &stats); err != nil {
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
			if _, _, err := m.store.readPackedBounded(entry.Hash, &entry, m.limits.BlobBytes); err == nil {
				continue
			}
			if _, err := readVerifiedLoosePath(m.layout.LoosePath(entry.Hash), entry.Hash, m.limits.BlobBytes); err != nil {
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
			if _, readErr := readVerifiedLoosePath(m.layout.LoosePath(hash), hash, m.limits.BlobBytes); readErr == nil {
				continue
			}
		}
		if _, err := reader.ReadBlob(hash); err != nil {
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
		data, source, found, readErr := m.readCandidate(ctx, candidate)
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
		id := pack.ComputeBlobID(data)
		if id.String() != candidate.Hash.String() {
			return fmt.Errorf("%w: appended hash differs from candidate", ErrContentMismatch)
		}
		frame, compressed := pack.EncodeFrame(data, pack.DefaultZstdLevel)
		if err := checkPlainOutput(m.limits, uint64(pack.MinEntryOffset), uint64(len(frame)), 1); err != nil {
			stats.BlobsDeferredOversized++
			continue
		}
		if writer != nil {
			if err := checkPlainOutput(m.limits, uint64(writer.StoredSize()), uint64(len(frame)), len(sources)+1); err != nil { //nolint:gosec // writer offsets are non-negative
				if err := seal(); err != nil {
					return err
				}
			}
		}
		if writer == nil {
			writer, err = pack.NewWriter(m.layout.PacksDir(), pack.WriterOptions{TargetSize: opts.TargetSize})
			if err != nil {
				return err
			}
		}
		entry, err := writer.AppendEncoded(id, frame, uint64(len(data)), compressed)
		if err != nil {
			return err
		}
		sources = append(sources, packedSource{candidate: candidate, path: source, entry: entry})
		rawBytes += int64(len(data))
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

func (m *Maintainer) readCandidate(ctx context.Context, candidate Candidate) ([]byte, string, bool, error) {
	var corrupt error
	for _, candidatePath := range candidate.Paths {
		if err := ctx.Err(); err != nil {
			return nil, "", false, err
		}
		path := candidatePath
		if !filepath.IsAbs(path) {
			path = filepath.Join(m.layout.Root(), filepath.FromSlash(path))
		}
		data, err := readVerifiedLoosePath(path, candidate.Hash, m.limits.BlobBytes)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			corrupt = errors.Join(corrupt, err)
			continue
		}
		return data, path, true, nil
	}
	return nil, "", false, corrupt
}

func readVerifiedLoosePath(path string, hash Hash, limit int64) ([]byte, error) {
	info, err := snapshotPathIdentity(path)
	if err != nil {
		return nil, err
	}
	if err := validateRegularNoFollow(path, info); err != nil {
		return nil, err
	}
	size := info.Size()
	if size < 0 || size > limit {
		return nil, newLimitError(LimitBlobRawBytes, uint64(size), uint64(limit)) //nolint:gosec
	}
	if uint64(size) > maxPlatformInt {
		return nil, newLimitError(LimitBlobRawBytes, uint64(size), maxPlatformInt)
	}
	f, err := openNoFollow(path, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data := make([]byte, int(size))
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, err
	}
	var probe [1]byte
	if n, err := f.Read(probe[:]); n != 0 || err == nil {
		return nil, ErrContentMismatch
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		return nil, errors.Join(err, errIdentityChanged)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != hash.String() {
		return nil, ErrContentMismatch
	}
	return data, nil
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
		_ = os.Remove(source.path)
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

func (m *Maintainer) sweepLoose(ctx context.Context, refs map[Hash]Reference, stats *PackStats) error {
	indexed, err := m.catalog.ListIndexed(ctx)
	if err != nil {
		return err
	}
	for _, entry := range indexed {
		path := m.layout.LoosePath(entry.Hash)
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if _, _, err := m.store.ReadBounded(ctx, entry.Hash, m.limits.BlobBytes); err != nil {
			continue
		}
		if _, err := readVerifiedLoosePath(path, entry.Hash, m.limits.BlobBytes); err != nil {
			continue
		}
		if err := os.Remove(path); err == nil {
			stats.LooseSwept++
		}
	}
	entries, err := os.ReadDir(m.layout.Root())
	if err != nil {
		return err
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
			hash, err := ParseHash(file.Name())
			if err != nil || hash.String()[:2] != shard.Name() || !file.Type().IsRegular() {
				continue
			}
			if _, live := refs[hash]; live {
				continue
			}
			if err := os.Remove(filepath.Join(m.layout.Root(), shard.Name(), file.Name())); err != nil {
				return err
			}
			stats.LooseOrphansRemoved++
		}
	}
	return nil
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
