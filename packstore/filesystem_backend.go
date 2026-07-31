package packstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.kenn.io/kit/pack"
)

// FilesystemBackendOptions configures one filesystem store binding. An absent
// ExpectedOwnership permits reads and ownership attachment, but all
// publication and retirement operations fail fenced until attachment succeeds.
type FilesystemBackendOptions struct {
	ExpectedOwnership *Ownership
	Limits            Limits
	ReaderSlots       int
}

// FilesystemBackend owns filesystem byte mechanics without granting logical
// membership or changing application catalog authority.
type FilesystemBackend struct {
	layout    Layout
	loose     *filesystemLooseStore
	reader    *Store
	ownership ownershipState
	limits    Limits
	legacy    bool
}

// NewFilesystemBackend prepares one filesystem store.
func NewFilesystemBackend(
	layout Layout,
	opts FilesystemBackendOptions,
) (*FilesystemBackend, error) {
	if layout.Root() == "" {
		return nil, fmt.Errorf("packstore: invalid empty layout")
	}
	if opts.ExpectedOwnership != nil {
		if err := opts.ExpectedOwnership.Validate(); err != nil {
			return nil, err
		}
	}
	loose, err := newFilesystemLooseStore(layout)
	if err != nil {
		return nil, err
	}
	reader, err := newStoreOptions(opts.Limits, opts.ReaderSlots)
	if err != nil {
		return nil, err
	}
	reader.layout = layout
	backend := newFilesystemBackendWithReader(layout, loose, reader, false)
	if opts.ExpectedOwnership != nil {
		backend.ownership.set(*opts.ExpectedOwnership)
	}
	return backend, nil
}

func newFilesystemBackendWithReader(
	layout Layout,
	loose *filesystemLooseStore,
	reader *Store,
	legacy bool,
) *FilesystemBackend {
	return &FilesystemBackend{
		layout: layout,
		loose:  loose,
		reader: reader,
		limits: reader.limits,
		legacy: legacy,
	}
}

// Layout returns the canonical filesystem layout.
func (b *FilesystemBackend) Layout() Layout { return b.layout }

// StoreID returns the store identity expected by this binding, or empty while
// unattached.
func (b *FilesystemBackend) StoreID() StoreID {
	expected := b.ownership.get()
	if expected == nil {
		return ""
	}
	return expected.Store
}

// Close releases cached pack descriptors.
func (b *FilesystemBackend) Close() error { return b.reader.Close() }

// Ownership reads and strictly validates the live canonical marker.
func (b *FilesystemBackend) Ownership(ctx context.Context) (Ownership, error) {
	if ctx == nil {
		return Ownership{}, fmt.Errorf("packstore: nil context")
	}
	if err := ctx.Err(); err != nil {
		return Ownership{}, err
	}
	return readOwnership(b.layout.OwnershipPath())
}

// ReplaceOwnership atomically publishes next after checking the expected prior
// marker. Filesystems do not provide a portable compare-and-swap rename, so
// concurrent operator takeovers are last-writer-wins and losers fence on their
// next fresh marker check.
func (b *FilesystemBackend) ReplaceOwnership(
	ctx context.Context,
	next Ownership,
	expected *Ownership,
) error {
	if ctx == nil {
		return fmt.Errorf("packstore: nil context")
	}
	if err := replaceOwnership(ctx, b.layout.OwnershipPath(), next, expected); err != nil {
		return err
	}
	b.ownership.set(next)
	return nil
}

func (b *FilesystemBackend) requireOwnership(ctx context.Context) (Ownership, error) {
	expected := b.ownership.get()
	if expected == nil {
		return Ownership{}, &OwnershipMismatchError{
			Err: fmt.Errorf("packstore: filesystem backend is not attached"),
		}
	}
	actual, err := b.Ownership(ctx)
	if err != nil {
		return Ownership{}, &OwnershipMismatchError{Expected: *expected, Err: err}
	}
	if actual != *expected {
		return Ownership{}, &OwnershipMismatchError{Expected: *expected, Actual: actual}
	}
	return actual, nil
}

// OpenLoose opens and verifies one loose representation. Read admission does
// not perform a marker round trip; applications cache fencing observations and
// fresh marker checks are required for destructive admission.
func (b *FilesystemBackend) OpenLoose(
	ctx context.Context,
	hash Hash,
	location LooseLocation,
) (VerifiedReadCloser, int64, error) {
	if err := location.validate(); err != nil {
		return nil, 0, err
	}
	var stream VerifiedReadCloser
	var size int64
	var err error
	if location.Encoding == 0 {
		stream, size, err = b.reader.openLooseStream(ctx, hash)
	} else {
		stream, size, err = b.reader.openLooseStreamAt(ctx, hash, location)
	}
	if err != nil {
		return nil, 0, classifyPhysicalError(err)
	}
	return &physicalVerifiedStream{stream: stream}, size, nil
}

// OpenPack opens and verifies one indexed pack entry.
func (b *FilesystemBackend) OpenPack(
	ctx context.Context,
	hash Hash,
	entry IndexEntry,
) (VerifiedReadCloser, int64, error) {
	stream, size, err := b.reader.openPackedStream(ctx, hash, &entry)
	if err != nil {
		return nil, 0, classifyPhysicalError(err)
	}
	return &physicalVerifiedStream{stream: stream}, size, nil
}

type physicalVerifiedStream struct {
	stream VerifiedReadCloser
}

func (s *physicalVerifiedStream) Read(p []byte) (int, error) {
	n, err := s.stream.Read(p)
	return n, classifyPhysicalError(err)
}

func (s *physicalVerifiedStream) Verify() error {
	return classifyPhysicalError(s.stream.Verify())
}

func (s *physicalVerifiedStream) Verified() bool {
	return s.stream.Verified()
}

func (s *physicalVerifiedStream) Close() error {
	return classifyPhysicalError(s.stream.Close())
}

func (b *FilesystemBackend) OpenSeekableLoose(
	ctx context.Context,
	hash Hash,
	location LooseLocation,
) (io.ReadSeekCloser, int64, error) {
	if err := location.validate(); err != nil {
		return nil, 0, err
	}
	if location.Encoding != 0 {
		stream, size, err := b.OpenLoose(ctx, hash, location)
		if err != nil {
			return nil, 0, err
		}
		return materializeSeekable(stream, size)
	}
	reader, size, err := b.reader.openSeekableLoose(ctx, hash)
	return reader, size, classifyPhysicalError(err)
}

func (b *FilesystemBackend) OpenSeekablePack(
	_ context.Context,
	hash Hash,
	entry IndexEntry,
) (io.ReadSeekCloser, int64, error) {
	reader, size, err := b.reader.openPacked(hash, &entry)
	return reader, size, classifyPhysicalError(err)
}

func (b *FilesystemBackend) ReadLooseBounded(
	ctx context.Context,
	hash Hash,
	location LooseLocation,
	maxBytes int64,
) ([]byte, int64, error) {
	if err := location.validate(); err != nil {
		return nil, 0, err
	}
	if location.Encoding != 0 {
		stream, size, err := b.OpenLoose(ctx, hash, location)
		if err != nil {
			return nil, 0, err
		}
		return consumeBounded(stream, size, maxBytes)
	}
	data, size, err := b.reader.readLooseBounded(ctx, hash, maxBytes)
	return data, size, classifyPhysicalError(err)
}

func (b *FilesystemBackend) ReadPackBounded(
	ctx context.Context,
	hash Hash,
	entry IndexEntry,
	maxBytes int64,
) ([]byte, int64, error) {
	data, size, err := b.reader.readPackedBounded(ctx, hash, &entry, maxBytes)
	return data, size, classifyPhysicalError(err)
}

// PublishLoose writes a canonical immutable loose object after a fresh
// ownership check.
func (b *FilesystemBackend) PublishLoose(
	ctx context.Context,
	hash Hash,
	src io.Reader,
	opts PublishOptions,
) (LooseReceipt, error) {
	owner, err := b.requireOwnership(ctx)
	if err != nil {
		return LooseReceipt{}, err
	}
	if src == nil {
		return LooseReceipt{}, fmt.Errorf("packstore: nil loose publication source")
	}
	if err := hash.Validate(); err != nil {
		return LooseReceipt{}, err
	}
	writeOpts, err := normalizePublishOptions(hash, opts)
	if err != nil {
		return LooseReceipt{}, err
	}
	generation, err := newLocationGeneration()
	if err != nil {
		return LooseReceipt{}, err
	}
	result, err := b.loose.Write(ctx, src, writeOpts)
	if err != nil {
		return LooseReceipt{}, err
	}
	return LooseReceipt{
		StoreID:    owner.Store,
		Generation: generation,
		Hash:       result.Hash,
		Location: LooseLocation{
			Encoding:    result.Encoding,
			LogicalSize: result.Size,
			StoredSize:  result.StoredSize,
		},
		Created: result.Created,
	}, nil
}

// RepairLoose deliberately overwrites one canonical loose object with bytes
// that are independently verified against its immutable logical identity.
func (b *FilesystemBackend) RepairLoose(
	ctx context.Context,
	hash Hash,
	src io.Reader,
	opts PublishOptions,
) (LooseReceipt, error) {
	owner, err := b.requireOwnership(ctx)
	if err != nil {
		return LooseReceipt{}, err
	}
	if src == nil {
		return LooseReceipt{}, fmt.Errorf("packstore: nil loose repair source")
	}
	if err := hash.Validate(); err != nil {
		return LooseReceipt{}, err
	}
	if !opts.SizeKnown || opts.ExpectedSize < 0 {
		return LooseReceipt{}, ErrInvalidPolicy
	}
	if opts.Durability == 0 {
		opts.Durability = DurablePublication
	}
	generation, err := newLocationGeneration()
	if err != nil {
		return LooseReceipt{}, err
	}
	result, err := b.loose.Repair(ctx, src, LooseIdentity{
		Hash: hash, Size: opts.ExpectedSize,
	}, RepairOptions{
		Durability: opts.Durability, Compression: opts.Compression,
		MaxBytes: opts.MaxBytes,
	})
	if err != nil {
		return LooseReceipt{}, err
	}
	return LooseReceipt{
		StoreID: owner.Store, Generation: generation, Hash: result.Hash,
		Location: LooseLocation{
			Encoding: result.Encoding, LogicalSize: result.Size,
			StoredSize: result.StoredSize,
		},
		Created: false,
	}, nil
}

func normalizePublishOptions(hash Hash, opts PublishOptions) (WriteOptions, error) {
	if opts.Durability == 0 {
		opts.Durability = DurablePublication
	}
	if opts.Dedup == 0 {
		opts.Dedup = VerifyFullHash
	}
	writeOpts := WriteOptions{
		Durability:   opts.Durability,
		Dedup:        opts.Dedup,
		ExpectedHash: hash,
		ExpectedSize: opts.ExpectedSize,
		SizeKnown:    opts.SizeKnown,
		MaxBytes:     opts.MaxBytes,
		Compression:  opts.Compression,
	}
	if err := validateWriteOptions(writeOpts); err != nil {
		return WriteOptions{}, err
	}
	return writeOpts, nil
}

// PublishPack writes a sealed plain pack and independently verifies every
// entry before returning a receipt. Existing identical canonical packs are
// adopted only after the same full read-back verification.
func (b *FilesystemBackend) PublishPack(
	ctx context.Context,
	packID string,
	src io.Reader,
	opts PublishOptions,
) (receipt PackReceipt, resultErr error) {
	owner, err := b.requireOwnership(ctx)
	if err != nil {
		return PackReceipt{}, err
	}
	if !pack.IsValidPackID(packID) {
		return PackReceipt{}, fmt.Errorf("packstore: invalid pack id %q", packID)
	}
	if src == nil {
		return PackReceipt{}, fmt.Errorf("packstore: nil pack publication source")
	}
	maxBytes, err := effectivePackPublicationLimit(
		opts.MaxBytes,
		opts.ExpectedSize,
		opts.SizeKnown,
		b.limits.PackBytes,
	)
	if err != nil {
		return PackReceipt{}, err
	}
	generation, err := newLocationGeneration()
	if err != nil {
		return PackReceipt{}, err
	}
	stagingDir := filepath.Join(b.layout.PacksDir(), ".staging")
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return PackReceipt{}, fmt.Errorf("packstore: prepare pack staging: %w", err)
	}
	staged, err := os.CreateTemp(stagingDir, ".pack-")
	if err != nil {
		return PackReceipt{}, fmt.Errorf("packstore: create pack staging: %w", err)
	}
	stagedPath := staged.Name()
	stagedOpen := true
	defer func() {
		if stagedOpen {
			resultErr = errors.Join(resultErr, staged.Close())
		}
		if err := os.Remove(stagedPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if err := staged.Chmod(0o600); err != nil {
		return PackReceipt{}, fmt.Errorf("packstore: protect pack staging: %w", err)
	}
	hasher := sha256.New()
	written, err := copyBoundedContext(ctx, io.MultiWriter(staged, hasher), src, maxBytes)
	if err != nil {
		return PackReceipt{}, err
	}
	var expectedDigest [sha256.Size]byte
	copy(expectedDigest[:], hasher.Sum(nil))
	if err := staged.Sync(); err != nil {
		return PackReceipt{}, fmt.Errorf("packstore: sync pack staging: %w", err)
	}
	if err := staged.Close(); err != nil {
		return PackReceipt{}, fmt.Errorf("packstore: close pack staging: %w", err)
	}
	stagedOpen = false
	final := b.layout.PackPath(packID)
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return PackReceipt{}, fmt.Errorf("packstore: prepare pack shard: %w", err)
	}
	created := false
	if err := publishLooseFile(stagedPath, final); err == nil {
		created = true
	} else if !errors.Is(err, fs.ErrExist) {
		return PackReceipt{}, fmt.Errorf("packstore: publish pack: %w", err)
	}
	size, err := verifyFilesystemPack(
		ctx,
		final,
		packID,
		b.limits,
		written,
		expectedDigest,
	)
	if err != nil {
		if errors.Is(err, ErrContentMismatch) {
			return PackReceipt{}, err
		}
		return PackReceipt{}, errors.Join(ErrPhysicalCorrupt, err)
	}
	if opts.Durability == DurablePublication || opts.Durability == 0 {
		if err := pack.SyncDir(filepath.Dir(final)); err != nil {
			return PackReceipt{}, fmt.Errorf("packstore: sync pack publication: %w", err)
		}
	}
	return PackReceipt{
		StoreID: owner.Store, Generation: generation,
		PackID: packID, Size: size, Created: created,
	}, nil
}

func copyBoundedContext(
	ctx context.Context,
	dst io.Writer,
	src io.Reader,
	maxBytes int64,
) (int64, error) {
	reader := io.Reader(&contextReader{ctx: ctx, reader: src})
	if maxBytes == 0 || maxBytes == math.MaxInt64 {
		return io.CopyBuffer(dst, reader, make([]byte, 64<<10))
	}
	limited := &io.LimitedReader{R: reader, N: maxBytes + 1}
	written, err := io.CopyBuffer(dst, limited, make([]byte, 64<<10))
	if err != nil {
		return written, fmt.Errorf("packstore: copy publication: %w", err)
	}
	if written > maxBytes {
		return written, newLimitError(
			LimitPackContainerBytes,
			uint64(written),  //nolint:gosec // written is non-negative
			uint64(maxBytes), //nolint:gosec // validated positive
		)
	}
	return written, nil
}

func effectivePackPublicationLimit(
	maxBytes int64,
	expectedSize int64,
	sizeKnown bool,
	configured int64,
) (int64, error) {
	if maxBytes < 0 || expectedSize < 0 || configured <= 0 {
		return 0, ErrInvalidPolicy
	}
	effective := configured
	if maxBytes > 0 {
		effective = min(maxBytes, configured)
	}
	if sizeKnown && expectedSize > effective {
		return 0, newLimitError(
			LimitPackContainerBytes,
			uint64(expectedSize), //nolint:gosec // validated non-negative
			uint64(effective),    //nolint:gosec // validated positive
		)
	}
	return effective, nil
}

func verifyFilesystemPack(
	ctx context.Context,
	path string,
	packID string,
	limits Limits,
	expectedSize int64,
	expectedDigest [sha256.Size]byte,
) (resultSize int64, resultErr error) {
	file, err := openNoFollow(path, false)
	if err != nil {
		return 0, err
	}
	info, err := file.Stat()
	if err != nil {
		return 0, errors.Join(err, file.Close())
	}
	if info.Size() != expectedSize {
		return 0, errors.Join(
			fmt.Errorf(
				"%w: canonical pack size %d does not match published size %d",
				ErrContentMismatch,
				info.Size(),
				expectedSize,
			),
			file.Close(),
		)
	}
	hasher := sha256.New()
	if _, err := io.CopyBuffer(hasher, file, make([]byte, 64<<10)); err != nil {
		return 0, errors.Join(err, file.Close())
	}
	var actualDigest [sha256.Size]byte
	copy(actualDigest[:], hasher.Sum(nil))
	if actualDigest != expectedDigest {
		return 0, errors.Join(
			fmt.Errorf("%w: canonical pack bytes differ", ErrContentMismatch),
			file.Close(),
		)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, errors.Join(err, file.Close())
	}
	reader, err := pack.NewReaderFromFileWithOptions(file, packID, nil, pack.ReaderOptions{
		Limits: pack.ReaderLimits{
			ContainerBytes: uint64(limits.PackBytes), //nolint:gosec
			FooterBytes:    uint64(limits.FooterBytes),
			Entries:        uint64(limits.PackEntries),
			RawBytes:       uint64(limits.BlobBytes),
			StoredBytes:    uint64(limits.BlobBytes),
			WindowBytes:    uint64(max(limits.BlobBytes, int64(1<<10))),
		},
	})
	if err != nil {
		return 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	for _, entry := range reader.Entries() {
		blob, err := reader.OpenBlob(ctx, entry)
		if err != nil {
			return 0, mapPackStreamLimit(err)
		}
		if err := errors.Join(blob.Verify(), blob.Close()); err != nil {
			return 0, mapPackStreamLimit(err)
		}
	}
	return info.Size(), nil
}

// Retire removes one canonical object after a fresh ownership check. Missing
// objects are successful and active readers retain their pinned descriptors.
func (b *FilesystemBackend) Retire(ctx context.Context, ref ObjectRef) error {
	if !b.legacy {
		if _, err := b.requireOwnership(ctx); err != nil {
			return err
		}
	}
	switch {
	case ref.LooseHash != "" && ref.PackID == "":
		return b.retireLoose(ref.LooseHash, ref.LooseEncoding)
	case ref.LooseHash == "" && ref.LooseEncoding == 0 && ref.PackID != "":
		return b.retirePack(ref.PackID)
	default:
		return fmt.Errorf("packstore: object reference must select exactly one representation")
	}
}

func (b *FilesystemBackend) retirePack(packID string) error {
	return b.reader.retireFilesystemPack(packID)
}

func (b *FilesystemBackend) retireLoose(hash Hash, encoding LooseEncoding) error {
	if err := hash.Validate(); err != nil {
		return err
	}
	var path string
	switch encoding {
	case LooseEncodingRaw:
		path = b.layout.LoosePath(hash)
	case LooseEncodingZstd:
		path = b.layout.CompressedLoosePath(hash)
	default:
		return fmt.Errorf("packstore: invalid loose retirement encoding %d", encoding)
	}
	_, err := removeLoosePath(path, openLooseIdentityPin)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("packstore: retire loose content: %w", err)
	}
	if err := pack.SyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("packstore: sync loose retirement: %w", err)
	}
	return nil
}

// Inventory returns a stable complete inventory of recognized canonical
// objects. Unknown names are reported and preserved.
func (b *FilesystemBackend) Inventory(
	ctx context.Context,
	cursor InventoryCursor,
) (InventoryPage, error) {
	if cursor != "" {
		return InventoryPage{}, fmt.Errorf("packstore: invalid filesystem inventory cursor")
	}
	var page InventoryPage
	err := filepath.WalkDir(b.layout.Root(), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(b.layout.Root(), path)
		if err != nil {
			return err
		}
		if relative == ownershipMarkerName {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if ref, ok := b.canonicalObjectRef(relative); ok {
			page.Objects = append(page.Objects, InventoryObject{Ref: ref, StoredSize: info.Size()})
		} else {
			page.Unknown = append(page.Unknown, filepath.ToSlash(relative))
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return page, nil
	}
	sort.Slice(page.Objects, func(i, j int) bool {
		return objectRefKey(page.Objects[i].Ref) < objectRefKey(page.Objects[j].Ref)
	})
	sort.Strings(page.Unknown)
	return page, err
}

// NamespaceEmpty reports whether the configured root contains any file or
// non-directory entry. Empty directory scaffolding is not physical authority.
func (b *FilesystemBackend) NamespaceEmpty(ctx context.Context) (bool, error) {
	empty := true
	err := filepath.WalkDir(b.layout.Root(), func(
		_ string, entry fs.DirEntry, walkErr error,
	) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			empty = false
			return fs.SkipAll
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("packstore: inspect filesystem namespace: %w", err)
	}
	return empty, nil
}

func (b *FilesystemBackend) canonicalObjectRef(relative string) (ObjectRef, bool) {
	slash := filepath.ToSlash(relative)
	if strings.HasPrefix(slash, "packs/") {
		base := filepath.Base(relative)
		packID := strings.TrimSuffix(base, PackExt)
		if strings.HasSuffix(base, PackExt) &&
			b.layout.PackPath(packID) == filepath.Join(b.layout.Root(), relative) {
			return ObjectRef{PackID: packID}, true
		}
		return ObjectRef{}, false
	}
	base := filepath.Base(relative)
	hashText := strings.TrimSuffix(base, ".zst")
	hash, err := ParseHash(hashText)
	if err != nil {
		return ObjectRef{}, false
	}
	canonical := b.layout.LoosePath(hash)
	if strings.HasSuffix(base, ".zst") {
		canonical = b.layout.CompressedLoosePath(hash)
	}
	if canonical != filepath.Join(b.layout.Root(), relative) {
		return ObjectRef{}, false
	}
	encoding := LooseEncodingRaw
	if strings.HasSuffix(base, ".zst") {
		encoding = LooseEncodingZstd
	}
	return ObjectRef{LooseHash: hash, LooseEncoding: encoding}, true
}

func objectRefKey(ref ObjectRef) string {
	if ref.LooseHash != "" {
		return fmt.Sprintf("loose/%d/%s", ref.LooseEncoding, ref.LooseHash)
	}
	return "pack/" + ref.PackID
}

func newLocationGeneration() (LocationGeneration, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("packstore: generate location generation: %w", err)
	}
	return LocationGeneration(hex.EncodeToString(value[:])), nil
}
