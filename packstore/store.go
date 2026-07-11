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

// StoreOptions configures mixed loose and packed reads.
type StoreOptions struct {
	Limits      Limits
	ReaderSlots int
}

// Store serves catalog-authorized content across loose and packed storage.
type Store struct {
	resolver Resolver
	layout   Layout
	limits   Limits
	slots    int

	// mu is held across packed pread/decode so eviction cannot close a reader
	// while another goroutine uses its descriptor.
	mu             sync.Mutex
	readers        map[string]*ordinaryPackReader
	boundedReaders map[string]*boundedPackReader
	order          []string
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
		readers:        make(map[string]*ordinaryPackReader),
		boundedReaders: make(map[string]*boundedPackReader),
	}, nil
}

// Open returns catalog-authorized content and its raw size. Resolution is
// retried once when a concurrent migration removes the initially selected
// physical source.
func (s *Store) Open(ctx context.Context, hash Hash) (io.ReadSeekCloser, int64, error) {
	if err := hash.Validate(); err != nil {
		return nil, 0, err
	}
	return resolveBlob(ctx, s, hash, s.openLoose, s.openPacked)
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
			return s.readPackedBounded(hash, entry, maxBytes)
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
	ids := make(map[string]struct{}, len(s.readers)+len(s.boundedReaders))
	for id := range s.readers {
		ids[id] = struct{}{}
	}
	for id := range s.boundedReaders {
		ids[id] = struct{}{}
	}
	var closeErr error
	for id := range ids {
		closeErr = errors.Join(closeErr, s.closePackSlotLocked(id))
	}
	s.order = nil
	return closeErr
}

// RetirePack closes cached readers and removes the canonical pack file. It
// deliberately does not alter catalog authority.
func (s *Store) RetirePack(packID string) error {
	if !pack.IsValidPackID(packID) {
		return fmt.Errorf("packstore: invalid pack id %q", packID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := s.order[:0]
	for _, id := range s.order {
		if id != packID {
			filtered = append(filtered, id)
		}
	}
	s.order = filtered
	closeErr := s.closePackSlotLocked(packID)
	removeErr := os.Remove(s.layout.PackPath(packID))
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	} else if removeErr != nil {
		removeErr = fmt.Errorf("packstore: remove pack %s: %w", packID, removeErr)
	}
	return errors.Join(closeErr, removeErr)
}

func (s *Store) openLoose(hash Hash) (io.ReadSeekCloser, int64, error) {
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
	if err := entry.Validate(); err != nil {
		return nil, 0, err
	}
	id, err := pack.ParseBlobID(hash.String())
	if err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	reader, err := s.readerLocked(entry.PackID)
	if err != nil {
		return nil, 0, err
	}
	entryIndex, found := reader.entryIndexes[id]
	if !found {
		return nil, 0, &fs.PathError{Op: "find blob in pack footer", Path: hash.String(), Err: fs.ErrNotExist}
	}
	footerEntry := reader.Entries()[entryIndex]
	if reader.ID() != entry.PackID || !packIndexMatchesFooter(entry, footerEntry) {
		return nil, 0, fmt.Errorf("packstore: pack index metadata mismatch for %s", hash)
	}
	data, err := reader.ReadBlob(footerEntry)
	if err != nil {
		return nil, 0, err
	}
	return nopSeekCloser{bytes.NewReader(data)}, int64(len(data)), nil
}

func (s *Store) readPackedBounded(hash Hash, entry *IndexEntry, maxBytes int64) ([]byte, int64, error) {
	if err := entry.Validate(); err != nil {
		return nil, 0, err
	}
	id, err := pack.ParseBlobID(hash.String())
	if err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	reader, err := s.boundedReaderLocked(entry.PackID)
	if err != nil {
		return nil, 0, err
	}
	footerEntry, found := reader.entries[id]
	if !found {
		return nil, 0, &fs.PathError{Op: "find blob in pack footer", Path: hash.String(), Err: fs.ErrNotExist}
	}
	if !packIndexMatchesFooter(entry, footerEntry) {
		return nil, 0, fmt.Errorf("packstore: pack index metadata mismatch for %s", hash)
	}
	data, err := reader.readBlob(footerEntry, maxBytes)
	return data, int64(len(data)), err
}

func packIndexMatchesFooter(index *IndexEntry, footer pack.Entry) bool {
	return index.Hash.String() == footer.ID.String() && index.Offset >= 0 && uint64(index.Offset) == footer.Offset &&
		index.StoredLen >= 0 && uint64(index.StoredLen) == footer.StoredLen &&
		index.RawLen >= 0 && uint64(index.RawLen) == footer.RawLen &&
		pack.BlobFlags(index.Flags) == footer.Flags && index.CRC32C == footer.CRC32C
}

type ordinaryPackReader struct {
	*pack.Reader
	entryIndexes map[pack.BlobID]int
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }
