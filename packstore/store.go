package packstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"

	"go.kenn.io/kit/pack"
)

const maxOpenReaders = 16

var createSeekableLooseTemp = createSeekableLooseTempPlatform

var copySeekableLoose = func(dst io.Writer, src io.Reader, buffer []byte) (int64, error) {
	return io.CopyBuffer(dst, src, buffer)
}

type physicalSourceNotFoundError struct{ err error }

func (e *physicalSourceNotFoundError) Error() string { return e.err.Error() }
func (e *physicalSourceNotFoundError) Unwrap() error { return e.err }

func markPhysicalSourceNotFound(err error) error {
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return &physicalSourceNotFoundError{err: err}
}

func isPhysicalSourceNotFound(err error) bool {
	var missing *physicalSourceNotFoundError
	return errors.As(err, &missing)
}

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
	// Limits applies to ReadBounded and packed OpenStream calls. Store.Open
	// retains its buffered compatibility behavior and does not enforce it.
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

	locationResolver LocationResolver
	backends         BackendRegistry
	health           *Health
	retryResolution  bool
	observeStreams   bool
	filesystem       *FilesystemBackend
	openLooseFile    func(string) (*os.File, fs.FileInfo, error)

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
	store, err := newStoreOptions(opts.Limits, opts.ReaderSlots)
	if err != nil {
		return nil, err
	}
	store.layout = layout
	store.resolver = resolver
	loose, err := newFilesystemLooseStore(layout)
	if err != nil {
		return nil, err
	}
	legacyBackend := newFilesystemBackendWithReader(layout, loose, store, true)
	store.filesystem = legacyBackend
	store.locationResolver = legacyLocationResolver{resolver: resolver}
	store.backends = legacyBackendRegistry{backend: legacyBackend}
	store.health = NewHealth()
	store.retryResolution = true
	return store, nil
}

func newStoreOptions(limits Limits, readerSlots int) (*Store, error) {
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	if readerSlots == 0 {
		readerSlots = maxOpenReaders
	}
	if readerSlots < 1 {
		return nil, fmt.Errorf("packstore: reader slots must be positive")
	}
	return &Store{
		limits: limits, slots: readerSlots, openLooseFile: openLooseFile,
		packReaders: make(map[string]*cachedPackReader),
	}, nil
}

// Open returns seekable catalog-authorized content and its logical size.
// Compressed loose objects are verified into a private temporary file to
// preserve this compatibility API; streaming callers should prefer OpenStream.
// Resolution is retried once when a concurrent migration removes the initially
// selected physical source.
func (s *Store) Open(ctx context.Context, hash Hash) (io.ReadSeekCloser, int64, error) {
	if ctx == nil {
		return nil, 0, fmt.Errorf("packstore: nil context")
	}
	if err := hash.Validate(); err != nil {
		return nil, 0, err
	}
	return s.openMultiSeekable(ctx, hash)
}

func (s *Store) openMultiSeekable(
	ctx context.Context,
	hash Hash,
) (io.ReadSeekCloser, int64, error) {
	reader, size, location, err := resolveCandidates(
		ctx,
		s,
		hash,
		func(backend ReadBackend, location ReadLocation) (io.ReadSeekCloser, int64, error) {
			if seekable, ok := backend.(seekableReadBackend); ok {
				if location.Loose != nil {
					return seekable.OpenSeekableLoose(ctx, hash, *location.Loose)
				}
				return seekable.OpenSeekablePack(ctx, hash, *location.Pack)
			}
			stream, streamSize, err := openBackendStream(ctx, backend, hash, location)
			if err != nil {
				return nil, 0, err
			}
			return materializeSeekable(stream, streamSize)
		},
	)
	if err != nil {
		return nil, 0, err
	}
	if s.observeStreams {
		s.health.Clear(hash, location)
	}
	return reader, size, nil
}

func materializeSeekable(
	stream VerifiedReadCloser,
	size int64,
) (io.ReadSeekCloser, int64, error) {
	temporary, err := createSeekableLooseTemp()
	if err != nil {
		return nil, 0, errors.Join(
			fmt.Errorf("packstore: create seekable temporary file: %w", err),
			stream.Close(),
		)
	}
	cleanup := func(primary error) error {
		return errors.Join(primary, stream.Close(), temporary.Close())
	}
	buffer := looseCopyBufferPool.Get().(*[looseCopyBufferBytes]byte)
	_, copyErr := copySeekableLoose(struct{ io.Writer }{temporary}, stream, buffer[:])
	looseCopyBufferPool.Put(buffer)
	if copyErr != nil {
		return nil, 0, cleanup(copyErr)
	}
	if err := stream.Verify(); err != nil {
		return nil, 0, cleanup(err)
	}
	if err := stream.Close(); err != nil {
		return nil, 0, cleanup(err)
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		return nil, 0, cleanup(fmt.Errorf("packstore: rewind seekable temporary file: %w", err))
	}
	return &temporarySeekCloser{File: temporary}, size, nil
}

// ReadBounded returns verified content while bounding both stored and raw
// allocations. Packed cache misses also preflight container and footer limits.
func (s *Store) ReadBounded(ctx context.Context, hash Hash, maxBytes int64) ([]byte, int64, error) {
	if ctx == nil {
		return nil, 0, fmt.Errorf("packstore: nil context")
	}
	if err := hash.Validate(); err != nil {
		return nil, 0, err
	}
	if maxBytes < 0 {
		return nil, 0, fmt.Errorf("packstore: bounded read limit must be nonnegative")
	}
	if maxBytes > s.limits.BlobBytes {
		maxBytes = s.limits.BlobBytes
	}
	return s.readMultiBounded(ctx, hash, maxBytes)
}

func (s *Store) readMultiBounded(
	ctx context.Context,
	hash Hash,
	maxBytes int64,
) ([]byte, int64, error) {
	data, size, location, err := resolveCandidates(
		ctx,
		s,
		hash,
		func(backend ReadBackend, location ReadLocation) ([]byte, int64, error) {
			if err := preflightBoundedStoredSize(location, maxBytes); err != nil {
				return nil, 0, err
			}
			if bounded, ok := backend.(boundedReadBackend); ok {
				if location.Loose != nil {
					return bounded.ReadLooseBounded(ctx, hash, *location.Loose, maxBytes)
				}
				return bounded.ReadPackBounded(ctx, hash, *location.Pack, maxBytes)
			}
			stream, streamSize, err := openBackendStream(ctx, backend, hash, location)
			if err != nil {
				return nil, 0, err
			}
			return consumeBounded(stream, streamSize, maxBytes)
		},
	)
	if err != nil {
		return nil, 0, err
	}
	if s.observeStreams {
		s.health.Clear(hash, location)
	}
	return data, size, nil
}

func preflightBoundedStoredSize(location ReadLocation, maxBytes int64) error {
	var logicalSize, storedSize int64
	if location.Loose != nil {
		logicalSize = location.Loose.LogicalSize
		storedSize = location.Loose.StoredSize
	} else {
		logicalSize = location.Pack.RawLen
		storedSize = location.Pack.StoredLen
	}
	// Leave catalog entries whose logical size is already over the limit to the
	// backend: a corrupt candidate must not prevent replica fallback. Stored
	// overhead is terminal only when the authorized logical content still fits.
	if logicalSize <= maxBytes && storedSize > maxBytes {
		return newLimitError(
			LimitBlobStoredBytes,
			uint64(storedSize), //nolint:gosec // locations reject negative sizes
			uint64(maxBytes),   //nolint:gosec // ReadBounded rejects negative limits
		)
	}
	return nil
}

func consumeBounded(
	stream VerifiedReadCloser,
	size int64,
	maxBytes int64,
) ([]byte, int64, error) {
	if size < 0 {
		return nil, 0, errors.Join(
			fmt.Errorf("packstore: negative backend content size %d", size),
			stream.Close(),
		)
	}
	if size > maxBytes {
		return nil, 0, errors.Join(
			newLimitError(LimitBlobRawBytes, uint64(size), uint64(maxBytes)), //nolint:gosec
			stream.Close(),
		)
	}
	if uint64(size) > maxPlatformInt {
		return nil, 0, errors.Join(
			newLimitError(LimitBlobRawBytes, uint64(size), maxPlatformInt),
			stream.Close(),
		)
	}
	data := make([]byte, int(size))
	_, readErr := io.ReadFull(stream, data)
	verifyErr := stream.Verify()
	closeErr := stream.Close()
	if err := errors.Join(readErr, verifyErr, closeErr); err != nil {
		return nil, 0, err
	}
	return data, size, nil
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
	if s.filesystem == nil {
		return fmt.Errorf("packstore: pack retirement requires a filesystem backend")
	}
	return s.filesystem.retirePack(packID)
}

func (s *Store) retireFilesystemPack(packID string) error {
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

type looseObject struct {
	file        *os.File
	encoding    LooseEncoding
	logicalSize int64
	storedSize  int64
}

func (s *Store) openLooseObject(hash Hash) (*looseObject, error) {
	object, err := s.openLooseObjectAt(hash, LooseEncodingZstd)
	if err == nil {
		return object, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return s.openLooseObjectAt(hash, LooseEncodingRaw)
}

func (s *Store) openLooseObjectAt(
	hash Hash,
	encoding LooseEncoding,
) (*looseObject, error) {
	switch encoding {
	case LooseEncodingZstd:
		f, info, err := s.openLooseFile(s.layout.CompressedLoosePath(hash))
		if err != nil {
			return nil, markPhysicalSourceNotFound(err)
		}
		header := make([]byte, compressedLooseHeaderSize)
		if _, readErr := io.ReadFull(f, header); readErr != nil {
			return nil, errors.Join(
				fmt.Errorf("%w: read compressed loose header: %v", ErrContentMismatch, readErr),
				f.Close(),
			)
		}
		logicalSize, headerErr := decodeCompressedLooseHeader(header)
		if headerErr != nil {
			return nil, errors.Join(
				fmt.Errorf("%w: decode compressed loose header: %w", ErrContentMismatch, headerErr),
				f.Close(),
			)
		}
		return &looseObject{
			file: f, encoding: LooseEncodingZstd,
			logicalSize: logicalSize, storedSize: info.Size(),
		}, nil
	case LooseEncodingRaw:
		f, info, err := s.openLooseFile(s.layout.LoosePath(hash))
		if err != nil {
			return nil, markPhysicalSourceNotFound(err)
		}
		return &looseObject{
			file: f, encoding: LooseEncodingRaw,
			logicalSize: info.Size(), storedSize: info.Size(),
		}, nil
	default:
		return nil, fmt.Errorf("packstore: invalid loose encoding %d", encoding)
	}
}

func openLooseFile(path string) (*os.File, fs.FileInfo, error) {
	f, err := openNoFollow(path, false)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, nil, errors.Join(err, f.Close())
	}
	if err := validateRegularNoFollow(path, info); err != nil {
		return nil, nil, errors.Join(err, f.Close())
	}
	return f, info, nil
}

func (s *Store) openSeekableLoose(ctx context.Context, hash Hash) (io.ReadSeekCloser, int64, error) {
	object, err := s.openLooseObject(hash)
	if err != nil {
		return nil, 0, err
	}
	if object.encoding == LooseEncodingRaw {
		return object.file, object.logicalSize, nil
	}
	stream, err := newLooseVerifiedStream(ctx, hash, object)
	if err != nil {
		return nil, 0, err
	}
	temporary, err := createSeekableLooseTemp()
	if err != nil {
		return nil, 0, errors.Join(
			fmt.Errorf("packstore: create seekable loose temporary file: %w", err),
			stream.Close(),
		)
	}
	cleanup := func(primary error) error {
		streamErr := stream.Close()
		closeErr := temporary.Close()
		return errors.Join(primary, streamErr, closeErr)
	}
	buffer := looseCopyBufferPool.Get().(*[looseCopyBufferBytes]byte)
	_, copyErr := copySeekableLoose(struct{ io.Writer }{temporary}, stream, buffer[:])
	looseCopyBufferPool.Put(buffer)
	if copyErr != nil {
		return nil, 0, cleanup(copyErr)
	}
	verifyErr := stream.Verify()
	if verifyErr != nil {
		return nil, 0, cleanup(verifyErr)
	}
	if err := stream.Close(); err != nil {
		return nil, 0, cleanup(err)
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		return nil, 0, cleanup(fmt.Errorf("packstore: rewind seekable loose temporary file: %w", err))
	}
	return &temporarySeekCloser{File: temporary}, object.logicalSize, nil
}

type temporarySeekCloser struct {
	*os.File
	once sync.Once
	err  error
}

func (f *temporarySeekCloser) Close() error {
	f.once.Do(func() {
		f.err = f.File.Close()
	})
	return f.err
}

func (s *Store) readLooseBounded(ctx context.Context, hash Hash, maxBytes int64) ([]byte, int64, error) {
	object, err := s.openLooseObject(hash)
	if err != nil {
		return nil, 0, err
	}
	size := object.logicalSize
	if size < 0 {
		return nil, 0, errors.Join(fmt.Errorf("packstore: negative loose size %d", size), object.file.Close())
	}
	if size > maxBytes {
		return nil, 0, errors.Join(
			newLimitError(LimitBlobRawBytes, uint64(size), uint64(maxBytes)), //nolint:gosec
			object.file.Close(),
		)
	}
	if object.storedSize > maxBytes {
		return nil, 0, errors.Join(
			newLimitError(LimitBlobStoredBytes, uint64(object.storedSize), uint64(maxBytes)), //nolint:gosec
			object.file.Close(),
		)
	}
	if uint64(size) > maxPlatformInt {
		return nil, 0, errors.Join(
			newLimitError(LimitBlobRawBytes, uint64(size), maxPlatformInt),
			object.file.Close(),
		)
	}
	stream, err := newLooseVerifiedStream(ctx, hash, object)
	if err != nil {
		return nil, 0, err
	}
	data := make([]byte, int(size))
	_, readErr := io.ReadFull(stream, data)
	verifyErr := stream.Verify()
	closeErr := stream.Close()
	if err := errors.Join(readErr, verifyErr, closeErr); err != nil {
		return nil, 0, err
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
			fmt.Errorf("%w: pack footer has no entry for %s", ErrContentMismatch, hash), release())
	}
	if !packIndexMatchesFooter(entry, footerEntry) {
		return nil, pack.Entry{}, nil, errors.Join(
			fmt.Errorf("%w: pack index metadata mismatch for %s", ErrContentMismatch, hash), release())
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
