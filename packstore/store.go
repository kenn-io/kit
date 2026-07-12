package packstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"

	"go.kenn.io/kit/pack"
)

const maxOpenReaders = 16

// ErrPackRetirementDeferred identifies a canonical pack removal that callers
// may retry after readers or external filesystem users release the file.
var ErrPackRetirementDeferred = errors.New("packstore: pack retirement deferred")

// PackRetirementError carries a retryable physical cleanup failure. Catalog
// authority is deliberately outside RetirePack and is never rolled back.
type PackRetirementError struct {
	PackID string
	Err    error
}

func (e *PackRetirementError) Error() string {
	return fmt.Sprintf("%s %s: %v", ErrPackRetirementDeferred, e.PackID, e.Err)
}

func (e *PackRetirementError) Unwrap() error { return e.Err }

func (e *PackRetirementError) Is(target error) bool {
	return target == ErrPackRetirementDeferred || errors.Is(e.Err, target)
}

// StoreOptions configures mixed loose and packed reads.
type StoreOptions struct {
	Limits Limits
	// ReaderSlots bounds cached pack descriptors. A stream whose slot is
	// evicted keeps its descriptor leased until terminal read or Close, so total
	// live descriptors are bounded by cached slots plus active streams.
	ReaderSlots int
}

// Store serves catalog-authorized content across loose and packed storage.
type Store struct {
	resolver Resolver
	layout   Layout
	limits   Limits
	slots    int

	// mu protects cache membership and descriptor leases. Content I/O never
	// holds it; retired descriptors close after their final lease is released.
	mu          sync.Mutex
	packReaders map[string]*cachedPackReader
	order       []string
}

// NewStore constructs a mixed content reader.
func NewStore(resolver Resolver, layout Layout, opts StoreOptions) (*Store, error) {
	if resolver == nil {
		return nil, fmt.Errorf("packstore: resolver is nil")
	}
	if layout.Root() == "" {
		return nil, fmt.Errorf("packstore: invalid empty layout")
	}
	if opts.Limits == (Limits{}) {
		opts.Limits = DefaultLimits()
	}
	if err := validateLimits(opts.Limits); err != nil {
		return nil, err
	}
	if opts.ReaderSlots == 0 {
		opts.ReaderSlots = maxOpenReaders
	}
	if opts.ReaderSlots < 1 {
		return nil, fmt.Errorf("packstore: reader slots must be positive")
	}
	return &Store{
		resolver: resolver, layout: layout, limits: opts.Limits, slots: opts.ReaderSlots,
		packReaders: make(map[string]*cachedPackReader),
	}, nil
}

// Open returns catalog-authorized content and its raw size. Resolution is
// retried once when a concurrent migration removes the initially selected
// physical source.
func (s *Store) Open(ctx context.Context, hash Hash) (io.ReadSeekCloser, int64, error) {
	if err := hash.Validate(); err != nil {
		return nil, 0, err
	}
	return resolveBlob(ctx, s, hash,
		func(hash Hash) (io.ReadSeekCloser, int64, error) { return s.openLoose(hash) },
		s.openPacked)
}

// ReadBounded returns verified content while bounding both stored and raw
// allocations. Packed cache misses also preflight container and footer limits.
func (s *Store) ReadBounded(ctx context.Context, hash Hash, maxBytes int64) ([]byte, int64, error) {
	if err := hash.Validate(); err != nil {
		return nil, 0, err
	}
	if maxBytes < 0 {
		return nil, 0, fmt.Errorf("packstore: bounded read limit must be nonnegative")
	}
	if maxBytes > s.limits.BlobBytes {
		maxBytes = s.limits.BlobBytes
	}
	return resolveBlob(ctx, s, hash,
		func(hash Hash) ([]byte, int64, error) { return s.readLooseBounded(hash, maxBytes) },
		func(hash Hash, entry *IndexEntry) ([]byte, int64, error) {
			return s.readPackedBounded(ctx, hash, entry, maxBytes)
		})
}

func resolveBlob[T any](ctx context.Context, store *Store, hash Hash,
	readLoose func(Hash) (T, int64, error),
	readPacked func(Hash, *IndexEntry) (T, int64, error),
) (T, int64, error) {
	var zero T
	location, err := store.resolver.Resolve(ctx, hash)
	if err != nil {
		return zero, 0, err
	}
	if !location.Member {
		return zero, 0, blobNotFound(hash)
	}
	if location.Pack == nil {
		value, size, looseErr := readLoose(hash)
		if !errors.Is(looseErr, fs.ErrNotExist) {
			return value, size, looseErr
		}
		location, err = store.resolver.Resolve(ctx, hash)
		if err != nil {
			return zero, 0, err
		}
		if !location.Member {
			return zero, 0, blobNotFound(hash)
		}
		if location.Pack == nil {
			return zero, 0, looseErr
		}
		return readPacked(hash, location.Pack)
	}
	value, size, packErr := readPacked(hash, location.Pack)
	if !errors.Is(packErr, fs.ErrNotExist) {
		return value, size, packErr
	}
	location, err = store.resolver.Resolve(ctx, hash)
	if err != nil {
		return zero, 0, err
	}
	if !location.Member {
		return zero, 0, blobNotFound(hash)
	}
	if location.Pack == nil {
		return readLoose(hash)
	}
	return readPacked(hash, location.Pack)
}

func blobNotFound(hash Hash) error {
	return &fs.PathError{Op: "open CAS blob", Path: hash.String(), Err: fs.ErrNotExist}
}

// Close releases all cached pack descriptors.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := append([]string(nil), s.order...)
	var closeErr error
	for _, id := range ids {
		closeErr = errors.Join(closeErr, s.retirePackSlotLocked(id))
	}
	s.order = nil
	return closeErr
}

// RetirePack retires cached readers and removes the canonical pack file. Live
// streams keep their exact descriptor until terminal read or Close. A physical
// removal failure returns PackRetirementError and may be retried. The method
// deliberately does not alter catalog authority.
func (s *Store) RetirePack(packID string) error {
	if !pack.IsValidPackID(packID) {
		return fmt.Errorf("packstore: invalid pack id %q", packID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	closeErr := s.retirePackSlotLocked(packID)
	removeErr := os.Remove(s.layout.PackPath(packID))
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	} else if removeErr != nil {
		removeErr = &PackRetirementError{PackID: packID, Err: removeErr}
	}
	return errors.Join(closeErr, removeErr)
}

func (s *Store) openLoose(hash Hash) (*os.File, int64, error) {
	f, err := openNoFollow(s.layout.LoosePath(hash), false)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	if err := validateRegularNoFollow(s.layout.LoosePath(hash), info); err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

func (s *Store) readLooseBounded(hash Hash, maxBytes int64) ([]byte, int64, error) {
	f, size, err := s.openLoose(hash)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	if size < 0 {
		return nil, 0, fmt.Errorf("packstore: negative loose size %d", size)
	}
	if size > maxBytes {
		return nil, 0, newLimitError(LimitBlobRawBytes, uint64(size), uint64(maxBytes)) //nolint:gosec
	}
	if uint64(size) > maxPlatformInt {
		return nil, 0, newLimitError(LimitBlobRawBytes, uint64(size), maxPlatformInt)
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, 0, err
	}
	var probe [1]byte
	n, probeErr := f.Read(probe[:])
	if n != 0 || probeErr == nil {
		return nil, 0, newLimitError(LimitBlobStatBytes, uint64(size)+uint64(n), uint64(size)) //nolint:gosec
	}
	if !errors.Is(probeErr, io.EOF) {
		return nil, 0, probeErr
	}
	sum := sha256.Sum256(data)
	if !bytes.Equal(sum[:], hash.Bytes()) {
		return nil, 0, fmt.Errorf("%w: loose hash differs from %s", ErrContentMismatch, hash)
	}
	return data, size, nil
}

func (s *Store) openPacked(hash Hash, entry *IndexEntry) (io.ReadSeekCloser, int64, error) {
	slot, footerEntry, release, err := s.acquirePackedEntry(hash, entry, false)
	if err != nil {
		return nil, 0, err
	}
	data, readErr := slot.reader.ReadBlob(footerEntry)
	err = errors.Join(readErr, release())
	if err != nil {
		return nil, 0, err
	}
	return nopSeekCloser{bytes.NewReader(data)}, int64(len(data)), nil
}

func (s *Store) readPackedBounded(
	ctx context.Context, hash Hash, entry *IndexEntry, maxBytes int64,
) (data []byte, size int64, resultErr error) {
	slot, footerEntry, release, err := s.acquirePackedEntry(hash, entry, true)
	if err != nil {
		return nil, 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	if err := s.validatePackPolicy(slot); err != nil {
		return nil, 0, err
	}
	limit := uint64(maxBytes) //nolint:gosec // validated non-negative by caller
	if footerEntry.RawLen > limit {
		return nil, 0, newLimitError(LimitBlobRawBytes, footerEntry.RawLen, limit)
	}
	if footerEntry.StoredLen > limit {
		return nil, 0, newLimitError(LimitBlobStoredBytes, footerEntry.StoredLen, limit)
	}
	if footerEntry.RawLen > maxPlatformInt {
		return nil, 0, newLimitError(LimitBlobRawBytes, footerEntry.RawLen, maxPlatformInt)
	}
	if footerEntry.StoredLen > maxPlatformInt {
		return nil, 0, newLimitError(LimitBlobStoredBytes, footerEntry.StoredLen, maxPlatformInt)
	}
	windowLimit := max(limit, uint64(1<<10))
	stream, err := slot.reader.OpenBlobWithOptions(ctx, footerEntry, pack.BlobReaderOptions{WindowBytes: windowLimit})
	if err != nil {
		return nil, 0, mapPackStreamLimit(err)
	}
	data = make([]byte, int(footerEntry.RawLen))
	_, readErr := io.ReadFull(stream, data)
	verifyErr := stream.Verify()
	closeErr := stream.Close()
	if err := errors.Join(readErr, verifyErr, closeErr); err != nil {
		return nil, 0, err
	}
	return data, int64(len(data)), nil
}

func (s *Store) acquirePackedEntry(
	hash Hash, entry *IndexEntry, enforcePolicy bool,
) (*cachedPackReader, pack.Entry, func() error, error) {
	if err := entry.Validate(); err != nil {
		return nil, pack.Entry{}, nil, err
	}
	id, err := pack.ParseBlobID(hash.String())
	if err != nil {
		return nil, pack.Entry{}, nil, err
	}
	slot, release, err := s.acquirePackReader(entry.PackID, enforcePolicy)
	if err != nil {
		return nil, pack.Entry{}, nil, err
	}
	footerEntry, found := slot.entries[id]
	if !found {
		return nil, pack.Entry{}, nil, errors.Join(
			&fs.PathError{Op: "find blob in pack footer", Path: hash.String(), Err: fs.ErrNotExist}, release())
	}
	if !packIndexMatchesFooter(entry, footerEntry) {
		return nil, pack.Entry{}, nil, errors.Join(
			fmt.Errorf("packstore: pack index metadata mismatch for %s", hash), release())
	}
	return slot, footerEntry, release, nil
}

func packIndexMatchesFooter(index *IndexEntry, footer pack.Entry) bool {
	return index.Hash.String() == footer.ID.String() && index.Offset >= 0 && uint64(index.Offset) == footer.Offset &&
		index.StoredLen >= 0 && uint64(index.StoredLen) == footer.StoredLen &&
		index.RawLen >= 0 && uint64(index.RawLen) == footer.RawLen &&
		pack.BlobFlags(index.Flags) == footer.Flags && index.CRC32C == footer.CRC32C
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }
