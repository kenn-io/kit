package packstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"go.kenn.io/kit/pack"
)

const looseCopyBufferBytes = 32 << 10

var looseCopyBufferPool = sync.Pool{
	New: func() any { return new([looseCopyBufferBytes]byte) },
}

var looseWriteStripes = func() [256]chan struct{} {
	var stripes [256]chan struct{}
	for index := range stripes {
		stripes[index] = make(chan struct{}, 1)
	}
	return stripes
}()

var (
	syncLooseFile             = func(file *os.File) error { return file.Sync() }
	snapshotLoosePathIdentity = snapshotPathIdentity
	newLooseZstdWriter        = func(dst io.Writer) (io.WriteCloser, error) {
		return zstd.NewWriter(dst, zstd.WithEncoderConcurrency(1))
	}
	newLooseZstdReader = func(src io.Reader) (looseZstdReader, error) {
		return zstd.NewReader(src,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(64<<20))
	}
	publishLooseFile        = os.Link
	beforeLoosePublish      = func(Hash, LooseEncoding) {}
	afterLooseStripeAcquire = func(Hash, LooseEncoding) {}
	closeLooseStagingFile   = func(file *os.File) error { return file.Close() }
	removeLooseStagingFile  = os.Remove
	syncLooseStagingDir     = func(path string) error { return pack.SyncDir(path) }
	chmodLooseStagingFile   = func(file *os.File, mode fs.FileMode) error { return file.Chmod(mode) }
)

var (
	// ErrInvalidPolicy reports an omitted or unknown physical-storage policy.
	ErrInvalidPolicy = errors.New("packstore: invalid loose storage policy")
	// ErrContentMismatch reports bytes, size, or an existing object that does
	// not agree with its content identity.
	ErrContentMismatch = errors.New("packstore: loose content mismatch")
	errIdentityChanged = errors.New("packstore: loose content changed identity")
)

// Durability selects the crash guarantee for loose publication.
type Durability uint8

const (
	// AtomicPublication publishes a complete file without requiring fsync.
	AtomicPublication Durability = iota + 1
	// DurablePublication fsyncs content and directory entries before success.
	DurablePublication
)

// DedupVerification selects how an existing canonical object is checked.
type DedupVerification uint8

const (
	// VerifyTypeAndSize checks structural identity without rereading content.
	VerifyTypeAndSize DedupVerification = iota + 1
	// VerifyFullHash streams the existing object through SHA-256.
	VerifyFullHash
)

// RemovalDurability selects whether a successful unlink is directory-synced.
type RemovalDurability uint8

const (
	// BestEffortRemoval unlinks without requiring the directory entry to be
	// crash-durable. It is suitable only while another authoritative copy exists.
	BestEffortRemoval RemovalDurability = iota + 1
	// DurableRemoval syncs the containing directory after unlink.
	DurableRemoval
)

// WriteOptions makes publication and dedup policy explicit. ExpectedHash is
// optional for store-directory staging and required for same-directory staging.
type WriteOptions struct {
	Durability   Durability
	Dedup        DedupVerification
	ExpectedHash Hash
	ExpectedSize int64
	SizeKnown    bool
	MaxBytes     int64
	Compression  LooseCompressionOptions
}

// WriteResult describes one canonical loose object.
type WriteResult struct {
	Hash       Hash
	Size       int64
	Path       string
	Created    bool
	Encoding   LooseEncoding
	StoredSize int64
}

// LooseStore owns policy-explicit loose content-addressed operations.
type LooseStore struct {
	layout Layout
}

// NewLooseStore prepares layout for loose operations.
func NewLooseStore(layout Layout) (*LooseStore, error) {
	if layout.Root() == "" {
		return nil, fmt.Errorf("packstore: invalid empty layout")
	}
	return &LooseStore{layout: layout}, nil
}

// Write streams src into its canonical content-addressed path.
func (s *LooseStore) Write(ctx context.Context, src io.Reader, opts WriteOptions) (WriteResult, error) {
	if err := validateWriteOptions(opts); err != nil {
		return WriteResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}

	if opts.ExpectedHash != "" && opts.SizeKnown {
		result, exists, err := s.existing(opts.ExpectedHash, opts.ExpectedSize, opts.Dedup, opts.Durability)
		if err != nil {
			return WriteResult{}, err
		}
		if exists {
			return result, nil
		}
	}

	stagingDir, err := s.stagingDir(opts)
	if err != nil {
		return WriteResult{}, err
	}
	return s.publish(ctx, src, opts, stagingDir, nil)
}

// WriteBytes publishes in-memory content without redundantly hashing it while
// copying. The caller must not mutate content until the method returns.
// Because identity is known before filesystem work begins, errors after that
// point return a result populated with Hash and Size.
func (s *LooseStore) WriteBytes(ctx context.Context, content []byte, opts WriteOptions) (WriteResult, error) {
	if err := validateWriteOptions(opts); err != nil {
		return WriteResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}

	sum := sha256.Sum256(content)
	hash, err := ParseHash(hex.EncodeToString(sum[:]))
	if err != nil {
		return WriteResult{}, err
	}
	size := int64(len(content))
	identity := &WriteResult{
		Hash:       hash,
		Size:       size,
		Path:       s.layout.LoosePath(hash),
		Encoding:   LooseEncodingRaw,
		StoredSize: size,
	}
	if opts.MaxBytes > 0 && size > opts.MaxBytes {
		return *identity, fmt.Errorf("%w: content is %d bytes, limit is %d", ErrContentMismatch, size, opts.MaxBytes)
	}
	if opts.ExpectedHash != "" && hash != opts.ExpectedHash {
		return *identity, fmt.Errorf("%w: expected hash %s, got %s", ErrContentMismatch, opts.ExpectedHash, hash)
	}
	if opts.SizeKnown && size != opts.ExpectedSize {
		return *identity, fmt.Errorf("%w: expected size %d, got %d", ErrContentMismatch, opts.ExpectedSize, size)
	}
	if result, exists, err := s.existing(hash, size, opts.Dedup, opts.Durability); err != nil {
		return *identity, err
	} else if exists {
		return result, nil
	}

	stagingDir := s.layout.LooseStagingDir(hash)
	return s.publish(ctx, bytes.NewReader(content), opts, stagingDir, identity)
}

type stagedLooseFile struct {
	file   *os.File
	path   string
	closed bool
}

type looseZstdReader interface {
	io.Reader
	Close()
}

func (s *LooseStore) publish(ctx context.Context, src io.Reader, opts WriteOptions, stagingDir string, known *WriteResult) (result WriteResult, resultErr error) {
	identity := WriteResult{}
	if known != nil {
		identity = *known
	}
	if err := ensureDirectory(stagingDir, opts.Durability); err != nil {
		return identity, fmt.Errorf("packstore: prepare loose staging: %w", err)
	}
	var staged []*stagedLooseFile
	defer func() {
		if err := cleanupLooseStaging(stagingDir, opts.Durability, staged...); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("packstore: clean loose staging: %w", err))
		}
	}()
	raw, err := createLooseStagingFile(stagingDir)
	if raw != nil {
		staged = append(staged, raw)
	}
	if err != nil {
		return identity, err
	}

	var compressed *stagedLooseFile
	var encoder io.WriteCloser
	if opts.Compression.Enabled {
		compressed, err = createLooseStagingFile(stagingDir)
		if compressed != nil {
			staged = append(staged, compressed)
		}
		if err != nil {
			return identity, err
		}
		if _, err := compressed.file.Write(make([]byte, compressedLooseHeaderSize)); err != nil {
			return identity, fmt.Errorf("packstore: write compressed loose header placeholder: %w", err)
		}
		encoder, err = newLooseZstdWriter(compressed.file)
		if err != nil {
			return identity, fmt.Errorf("packstore: create loose zstd encoder: %w", err)
		}
	}

	hasher := sha256.New()
	writers := []io.Writer{raw.file}
	if known == nil {
		writers = append(writers, hasher)
	}
	if encoder != nil {
		writers = append(writers, encoder)
	}
	reader := io.Reader(&contextReader{ctx: ctx, reader: src})
	if opts.MaxBytes > 0 && opts.MaxBytes < math.MaxInt64 {
		reader = io.LimitReader(reader, opts.MaxBytes+1)
	}
	buffer := looseCopyBufferPool.Get().(*[looseCopyBufferBytes]byte)
	size, copyErr := io.CopyBuffer(io.MultiWriter(writers...), reader, buffer[:])
	looseCopyBufferPool.Put(buffer)
	if encoder != nil {
		err = encoder.Close()
	}
	if copyErr != nil || err != nil {
		return identity, fmt.Errorf("packstore: stage loose content: %w", errors.Join(copyErr, err))
	}
	if err := ctx.Err(); err != nil {
		return identity, err
	}
	if opts.MaxBytes > 0 && size > opts.MaxBytes {
		return identity, fmt.Errorf("%w: content is %d bytes, limit is %d", ErrContentMismatch, size, opts.MaxBytes)
	}
	if known == nil {
		hash, err := ParseHash(hex.EncodeToString(hasher.Sum(nil)))
		if err != nil {
			return identity, err
		}
		identity = WriteResult{Hash: hash, Size: size}
	} else if size != identity.Size {
		return identity, fmt.Errorf("%w: content changed size from %d to %d", ErrContentMismatch, identity.Size, size)
	}
	if opts.ExpectedHash != "" && identity.Hash != opts.ExpectedHash {
		return identity, fmt.Errorf("%w: expected hash %s, got %s", ErrContentMismatch, opts.ExpectedHash, identity.Hash)
	}
	if opts.SizeKnown && identity.Size != opts.ExpectedSize {
		return identity, fmt.Errorf("%w: expected size %d, got %d", ErrContentMismatch, opts.ExpectedSize, identity.Size)
	}

	rawStoredSize := identity.Size
	if compressed != nil {
		header := encodeCompressedLooseHeader(uint64(identity.Size))
		if _, err := compressed.file.WriteAt(header[:], 0); err != nil {
			return identity, fmt.Errorf("packstore: finalize compressed loose header: %w", err)
		}
		info, err := compressed.file.Stat()
		if err != nil {
			return identity, fmt.Errorf("packstore: stat compressed loose staging: %w", err)
		}
		compressedSize := info.Size()
		if shouldCompressLoose(identity.Size, compressedSize, opts.Compression) {
			identity.Path = s.layout.CompressedLoosePath(identity.Hash)
			identity.Encoding = LooseEncodingZstd
			identity.StoredSize = compressedSize
		} else {
			identity.Path = s.layout.LoosePath(identity.Hash)
			identity.Encoding = LooseEncodingRaw
			identity.StoredSize = rawStoredSize
		}
	} else {
		identity.Path = s.layout.LoosePath(identity.Hash)
		identity.Encoding = LooseEncodingRaw
		identity.StoredSize = rawStoredSize
	}

	selected := raw
	if identity.Encoding == LooseEncodingZstd {
		selected = compressed
	}
	if opts.Durability == DurablePublication {
		if err := syncLooseFile(selected.file); err != nil {
			return identity, fmt.Errorf("packstore: sync loose staging file: %w", err)
		}
	}
	if err := selected.close(); err != nil {
		return identity, fmt.Errorf("packstore: close loose staging file: %w", err)
	}

	beforeLoosePublish(identity.Hash, identity.Encoding)
	releaseStripe, err := acquireLooseWriteStripe(ctx, identity.Hash)
	if err != nil {
		return identity, err
	}
	defer releaseStripe()
	afterLooseStripeAcquire(identity.Hash, identity.Encoding)
	if err := ctx.Err(); err != nil {
		return identity, err
	}
	existing, exists, err := s.existing(identity.Hash, identity.Size, opts.Dedup, opts.Durability)
	if err != nil {
		return identity, err
	}
	if err := ctx.Err(); err != nil {
		return identity, err
	}
	if exists {
		return existing, nil
	}

	final := identity.Path
	shard := filepath.Dir(final)
	if filepath.Clean(shard) != filepath.Clean(stagingDir) {
		if err := ensureDirectory(shard, opts.Durability); err != nil {
			return identity, fmt.Errorf("packstore: prepare loose shard: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return identity, err
	}
	if err := publishLooseFile(selected.path, final); err != nil {
		result, exists, verifyErr := s.existing(identity.Hash, identity.Size, opts.Dedup, opts.Durability)
		if verifyErr == nil && exists {
			return result, nil
		}
		return identity, errors.Join(fmt.Errorf("packstore: publish loose content: %w", err), verifyErr)
	}
	identity.Created = true
	if opts.Durability == DurablePublication {
		if err := pack.SyncDir(shard); err != nil {
			return identity, fmt.Errorf("packstore: sync loose shard: %w", err)
		}
	}
	return identity, nil
}

func acquireLooseWriteStripe(ctx context.Context, hash Hash) (func(), error) {
	stripe := looseWriteStripes[looseHashLockIndex(hash)]
	select {
	case stripe <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-stripe
			return nil, err
		}
		return func() { <-stripe }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func looseHashLockIndex(hash Hash) byte {
	return hexNibble(hash[0])<<4 | hexNibble(hash[1])
}

func hexNibble(value byte) byte {
	if value >= '0' && value <= '9' {
		return value - '0'
	}
	return value - 'a' + 10
}

func createLooseStagingFile(dir string) (*stagedLooseFile, error) {
	file, err := os.CreateTemp(dir, ".staging-")
	if err != nil {
		return nil, fmt.Errorf("packstore: create loose staging file: %w", err)
	}
	staged := &stagedLooseFile{file: file, path: file.Name()}
	if err := chmodLooseStagingFile(file, 0o600); err != nil {
		return staged, fmt.Errorf("packstore: chmod loose staging file: %w", err)
	}
	return staged, nil
}

func cleanupLooseStaging(dir string, durability Durability, staged ...*stagedLooseFile) error {
	var cleanupErr error
	var unlinksAttempted bool
	for _, file := range staged {
		attempted, err := file.cleanup()
		unlinksAttempted = unlinksAttempted || attempted
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if durability == DurablePublication && unlinksAttempted {
		cleanupErr = errors.Join(cleanupErr, syncLooseStagingDir(dir))
	}
	return cleanupErr
}

func (f *stagedLooseFile) cleanup() (bool, error) {
	if f == nil {
		return false, nil
	}
	closeErr := f.close()
	var removeErr error
	unlinkAttempted := f.path != ""
	if f.path != "" {
		removeErr = removeLooseStagingFile(f.path)
		if errors.Is(removeErr, fs.ErrNotExist) {
			removeErr = nil
		}
		if removeErr == nil {
			f.path = ""
		}
	}
	return unlinkAttempted, errors.Join(closeErr, removeErr)
}

func (f *stagedLooseFile) close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	return closeLooseStagingFile(f.file)
}

func shouldCompressLoose(logicalSize, storedSize int64, opts LooseCompressionOptions) bool {
	if !opts.Enabled || logicalSize < opts.MinBytes || storedSize > logicalSize {
		return false
	}
	whole := logicalSize / 100 * int64(opts.MinSavingsPercent)
	remainder := logicalSize % 100 * int64(opts.MinSavingsPercent)
	requiredSavings := whole + (remainder+99)/100
	return logicalSize-storedSize >= requiredSavings
}

// Verify checks whether the canonical loose object exists and satisfies the
// requested identity, deduplication, and durability policy.
func (s *LooseStore) Verify(hash Hash, size int64, verification DedupVerification, durability Durability) (WriteResult, bool, error) {
	if err := hash.Validate(); err != nil {
		return WriteResult{}, false, err
	}
	opts := WriteOptions{
		Durability:   durability,
		Dedup:        verification,
		ExpectedHash: hash,
		ExpectedSize: size,
		SizeKnown:    true,
	}
	if err := validateWriteOptions(opts); err != nil {
		return WriteResult{}, false, err
	}
	return s.existing(hash, size, verification, durability)
}

// Remove deletes a canonical loose object. Missing objects are successful.
func (s *LooseStore) Remove(hash Hash, durability RemovalDurability) error {
	if err := hash.Validate(); err != nil {
		return err
	}
	if durability != BestEffortRemoval && durability != DurableRemoval {
		return ErrInvalidPolicy
	}
	path := s.layout.LoosePath(hash)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("packstore: remove loose content: %w", err)
	}
	if durability == DurableRemoval {
		if err := pack.SyncDir(filepath.Dir(path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("packstore: sync loose removal: %w", err)
		}
	}
	return nil
}

func validateWriteOptions(opts WriteOptions) error {
	if opts.Durability != AtomicPublication && opts.Durability != DurablePublication {
		return ErrInvalidPolicy
	}
	if opts.Dedup != VerifyTypeAndSize && opts.Dedup != VerifyFullHash {
		return ErrInvalidPolicy
	}
	if opts.ExpectedHash != "" {
		if err := opts.ExpectedHash.Validate(); err != nil {
			return err
		}
	}
	if opts.ExpectedSize < 0 || opts.MaxBytes < 0 {
		return ErrInvalidPolicy
	}
	if opts.Compression.MinBytes < 0 || opts.Compression.MinSavingsPercent < 0 || opts.Compression.MinSavingsPercent > 100 {
		return ErrInvalidPolicy
	}
	if opts.SizeKnown && opts.MaxBytes > 0 && opts.ExpectedSize > opts.MaxBytes {
		return fmt.Errorf("%w: expected size is %d bytes, limit is %d", ErrContentMismatch, opts.ExpectedSize, opts.MaxBytes)
	}
	return nil
}

func (s *LooseStore) stagingDir(opts WriteOptions) (string, error) {
	if s.layout.staging == StagingSameDirectory {
		if opts.ExpectedHash == "" {
			return "", fmt.Errorf("%w: same-directory staging requires expected hash", ErrInvalidPolicy)
		}
		return s.layout.LooseStagingDir(opts.ExpectedHash), nil
	}
	return filepath.Join(s.layout.Root(), s.layout.stagingDir), nil
}

func (s *LooseStore) existing(hash Hash, size int64, verification DedupVerification, durability Durability) (WriteResult, bool, error) {
	result, exists, err := s.existingPath(s.layout.CompressedLoosePath(hash), hash, size, LooseEncodingZstd, verification, durability)
	if err != nil || exists {
		return result, exists, err
	}
	return s.existingPath(s.layout.LoosePath(hash), hash, size, LooseEncodingRaw, verification, durability)
}

func (s *LooseStore) existingPath(path string, hash Hash, size int64, encoding LooseEncoding, verification DedupVerification, durability Durability) (WriteResult, bool, error) {
	const maxIdentityAttempts = 8
	var result WriteResult
	var exists bool
	var err error
	for attempt := range maxIdentityAttempts {
		result, exists, err = s.existingOnce(path, hash, size, encoding, verification, durability)
		if !errors.Is(err, errIdentityChanged) {
			return result, exists, err
		}
		if durability == DurablePublication {
			return result, exists, err
		}
		if attempt != maxIdentityAttempts-1 {
			runtime.Gosched()
			time.Sleep(time.Duration(attempt+1) * time.Millisecond)
		}
	}
	return result, exists, err
}

func (s *LooseStore) existingOnce(path string, hash Hash, size int64, encoding LooseEncoding, verification DedupVerification, durability Durability) (WriteResult, bool, error) {
	info, err := snapshotLoosePathIdentity(path)
	if errors.Is(err, fs.ErrNotExist) {
		return WriteResult{}, false, nil
	}
	if err != nil {
		return WriteResult{}, false, fmt.Errorf("packstore: inspect loose content: %w", err)
	}
	if err := validateRegularNoFollow(path, info); err != nil {
		return WriteResult{}, false, err
	}
	if encoding == LooseEncodingZstd {
		if err := s.verifyCompressedPath(path, info, hash, size, verification, durability == DurablePublication); err != nil {
			return WriteResult{}, false, err
		}
	} else {
		if info.Size() != size {
			return WriteResult{}, false, fmt.Errorf("%w: existing size is %d, want %d", ErrContentMismatch, info.Size(), size)
		}
		if verification == VerifyFullHash {
			if err := s.verifyPathHash(path, info, hash, durability == DurablePublication); err != nil {
				return WriteResult{}, false, err
			}
		} else if durability == DurablePublication {
			if err := syncPathIdentity(path, info); err != nil {
				return WriteResult{}, false, err
			}
		}
	}
	if durability == DurablePublication {
		if err := pack.SyncDir(s.layout.Root()); err != nil {
			return WriteResult{}, false, fmt.Errorf("packstore: sync loose root: %w", err)
		}
		if err := pack.SyncDir(filepath.Dir(path)); err != nil {
			return WriteResult{}, false, fmt.Errorf("packstore: sync existing loose shard: %w", err)
		}
	}
	return WriteResult{
		Hash:       hash,
		Size:       size,
		Path:       path,
		Encoding:   encoding,
		StoredSize: info.Size(),
	}, true, nil
}

func (s *LooseStore) verifyCompressedPath(path string, before fs.FileInfo, expectedHash Hash, expectedSize int64, verification DedupVerification, durable bool) error {
	f, err := openNoFollow(path, durable)
	if err != nil {
		return fmt.Errorf("packstore: open compressed loose content: %w", err)
	}
	descriptorInfo, statErr := f.Stat()
	if statErr != nil {
		return errors.Join(statErr, f.Close())
	}
	if !os.SameFile(before, descriptorInfo) {
		return errors.Join(fmt.Errorf("%w: %w", ErrContentMismatch, errIdentityChanged), f.Close())
	}
	header := make([]byte, compressedLooseHeaderSize)
	if _, err := io.ReadFull(f, header); err != nil {
		return errors.Join(fmt.Errorf("%w: read compressed loose header: %v", ErrContentMismatch, err), f.Close())
	}
	logicalSize, err := decodeCompressedLooseHeader(header)
	if err != nil {
		return errors.Join(fmt.Errorf("%w: %v", ErrContentMismatch, err), f.Close())
	}
	if logicalSize != expectedSize {
		return errors.Join(fmt.Errorf("%w: existing logical size is %d, want %d", ErrContentMismatch, logicalSize, expectedSize), f.Close())
	}
	if verification == VerifyFullHash {
		decoder, err := newLooseZstdReader(struct{ io.Reader }{f})
		if err != nil {
			return errors.Join(fmt.Errorf("%w: open compressed loose payload: %v", ErrContentMismatch, err), f.Close())
		}
		hasher := sha256.New()
		buffer := looseCopyBufferPool.Get().(*[looseCopyBufferBytes]byte)
		decodedSize, readErr := io.CopyBuffer(hasher, &io.LimitedReader{R: decoder, N: expectedSize}, buffer[:])
		looseCopyBufferPool.Put(buffer)
		if readErr == nil && decodedSize == expectedSize {
			var extra [1]byte
			n, probeErr := io.ReadFull(decoder, extra[:])
			if n != 0 {
				readErr = fmt.Errorf("decoded size exceeds expected %d bytes", expectedSize)
			} else if probeErr != nil && !errors.Is(probeErr, io.EOF) && !errors.Is(probeErr, io.ErrUnexpectedEOF) {
				readErr = probeErr
			}
		}
		decoder.Close()
		if readErr != nil {
			return errors.Join(fmt.Errorf("%w: decode compressed loose payload: %v", ErrContentMismatch, readErr), f.Close())
		}
		if decodedSize != expectedSize {
			return errors.Join(fmt.Errorf("%w: decoded size is %d, want %d", ErrContentMismatch, decodedSize, expectedSize), f.Close())
		}
		if hex.EncodeToString(hasher.Sum(nil)) != expectedHash.String() {
			return errors.Join(fmt.Errorf("%w: existing hash differs from %s", ErrContentMismatch, expectedHash), f.Close())
		}
	}
	if durable {
		if err := syncLooseFile(f); err != nil {
			return errors.Join(err, f.Close())
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	after, err := snapshotLoosePathIdentity(path)
	if err != nil {
		return fmt.Errorf("packstore: recheck compressed loose content: %w", err)
	}
	if !os.SameFile(before, descriptorInfo) || !os.SameFile(after, descriptorInfo) {
		return fmt.Errorf("%w: %w", ErrContentMismatch, errIdentityChanged)
	}
	return nil
}

func (s *LooseStore) verifyPathHash(path string, before fs.FileInfo, expected Hash, durable bool) error {
	f, err := openNoFollow(path, durable)
	if err != nil {
		return fmt.Errorf("packstore: open loose content: %w", err)
	}
	descriptorInfo, statErr := f.Stat()
	if statErr != nil {
		return errors.Join(statErr, f.Close())
	}
	if !os.SameFile(before, descriptorInfo) {
		return errors.Join(fmt.Errorf("%w: %w", ErrContentMismatch, errIdentityChanged), f.Close())
	}

	hasher := sha256.New()
	buffer := looseCopyBufferPool.Get().(*[looseCopyBufferBytes]byte)
	_, readErr := io.CopyBuffer(hasher, struct{ io.Reader }{f}, buffer[:])
	looseCopyBufferPool.Put(buffer)
	if readErr == nil && hex.EncodeToString(hasher.Sum(nil)) != expected.String() {
		readErr = fmt.Errorf("%w: existing hash differs from %s", ErrContentMismatch, expected)
	}
	if readErr == nil && durable {
		readErr = syncLooseFile(f)
	}
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	after, err := snapshotLoosePathIdentity(path)
	if err != nil {
		return fmt.Errorf("packstore: recheck loose content: %w", err)
	}
	if !os.SameFile(before, descriptorInfo) || !os.SameFile(after, descriptorInfo) {
		return fmt.Errorf("%w: %w", ErrContentMismatch, errIdentityChanged)
	}
	return nil
}

func syncPathIdentity(path string, before fs.FileInfo) error {
	f, err := openNoFollow(path, true)
	if err != nil {
		return fmt.Errorf("packstore: open existing loose content durably: %w", err)
	}
	descriptorInfo, statErr := f.Stat()
	if statErr != nil || !os.SameFile(before, descriptorInfo) {
		return errors.Join(statErr, fmt.Errorf("%w: %w", ErrContentMismatch, errIdentityChanged), f.Close())
	}
	syncErr := syncLooseFile(f)
	closeErr := f.Close()
	if syncErr != nil || closeErr != nil {
		return errors.Join(syncErr, closeErr)
	}
	after, err := snapshotLoosePathIdentity(path)
	if err != nil {
		return fmt.Errorf("packstore: recheck durable loose content: %w", err)
	}
	if !os.SameFile(descriptorInfo, after) {
		return fmt.Errorf("%w: %w", ErrContentMismatch, errIdentityChanged)
	}
	return nil
}

func validateRegularNoFollow(path string, info fs.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not an independent regular file", ErrContentMismatch, path)
	}
	if err := validatePlatformFileInfo(info); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrContentMismatch, path, err)
	}
	return nil
}

func ensureDirectory(path string, durability Durability) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if durability == DurablePublication {
			return pack.MkdirAllSynced(path)
		}
		return os.MkdirAll(path, 0o700)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not an independent directory", path)
	}
	if durability == DurablePublication {
		if err := pack.SyncDir(filepath.Dir(path)); err != nil {
			return fmt.Errorf("packstore: sync loose directory parent: %w", err)
		}
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
