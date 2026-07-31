package packstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestFilesystemBackendPublishesAndInventoriesCanonicalObjects(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	looseContent := []byte("inventory loose content")
	looseHash := hashForTest(looseContent)
	looseReceipt, err := backend.PublishLoose(
		ctx,
		looseHash,
		bytes.NewReader(looseContent),
		PublishOptions{ExpectedSize: int64(len(looseContent)), SizeKnown: true},
	)
	require.NoError(err)

	packPath, packID, entries := buildBackendPackSource(t, []byte("inventory packed content"))
	packSource, err := os.Open(packPath)
	require.NoError(err)
	packReceipt, err := backend.PublishPack(ctx, packID, packSource, PublishOptions{})
	require.NoError(errors.Join(err, packSource.Close()))
	assert.True(packReceipt.Created)
	require.Len(entries, 1)
	indexed, err := indexEntryFromPack(entries[0], packID)
	require.NoError(err)
	stream, _, err := backend.OpenPack(ctx, indexed.Hash, indexed)
	require.NoError(err)
	got, err := io.ReadAll(stream)
	require.NoError(err)
	require.NoError(stream.Close())
	assert.Equal([]byte("inventory packed content"), got)

	require.NoError(os.WriteFile(filepath.Join(backend.Layout().Root(), "operator-note"), []byte("preserve"), 0o600))
	page, err := backend.Inventory(ctx, "")
	require.NoError(err)
	assert.ElementsMatch(
		[]InventoryObject{
			{
				Ref: ObjectRef{
					LooseHash: looseHash, LooseEncoding: looseReceipt.Location.Encoding,
				},
				StoredSize: looseReceipt.Location.StoredSize,
			},
			{Ref: ObjectRef{PackID: packID}, StoredSize: packReceipt.Size},
		},
		page.Objects,
	)
	assert.Equal([]string{"operator-note"}, page.Unknown)
}

func TestFilesystemBackendInventoryRejectsCanonicalSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	hash := hashForTest([]byte("unsafe inventory entry"))
	path := backend.Layout().LoosePath(hash)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(target, []byte("not authoritative"), 0o600))
	require.NoError(t, os.Symlink(target, path))
	relative, err := filepath.Rel(backend.Layout().Root(), path)
	require.NoError(t, err)

	page, err := backend.Inventory(context.Background(), "")

	require.NoError(t, err)
	assert.Empty(t, page.Objects)
	assert.Equal(t, []string{filepath.ToSlash(relative)}, page.Unknown)
}

func TestFilesystemBackendInventoryFollowsConfiguredSymlinkRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	require := require.New(t)
	actualRoot := t.TempDir()
	linkedRoot := filepath.Join(t.TempDir(), "store")
	require.NoError(os.Symlink(actualRoot, linkedRoot))
	layout, err := NewLayout(linkedRoot, LayoutOptions{
		Staging: StagingStoreDirectory, StagingDir: "tmp",
	})
	require.NoError(err)
	backend, err := NewFilesystemBackend(layout, FilesystemBackendOptions{})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(backend.Close()) })
	empty, err := backend.NamespaceEmpty(context.Background())
	require.NoError(err)
	assert.True(t, empty)
	content := []byte("symlink-root inventory content")
	hash := hashForTest(content)
	require.NoError(os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
	require.NoError(os.WriteFile(layout.LoosePath(hash), content, 0o600))

	page, err := backend.Inventory(context.Background(), "")
	require.NoError(err)
	empty, err = backend.NamespaceEmpty(context.Background())
	require.NoError(err)

	assert.Equal(t, []InventoryObject{{
		Ref:        ObjectRef{LooseHash: hash, LooseEncoding: LooseEncodingRaw},
		StoredSize: int64(len(content)),
	}}, page.Objects)
	assert.Empty(t, page.Unknown)
	assert.False(t, empty)
}

func TestFilesystemWalkPropagatesMissingEntryAfterRootResolution(t *testing.T) {
	layout := layoutForStoreTest(t)
	backend, err := NewFilesystemBackend(layout, FilesystemBackendOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	originalWalk := walkFilesystemTree
	walkFilesystemTree = func(string, fs.WalkDirFunc) error { return fs.ErrNotExist }
	t.Cleanup(func() { walkFilesystemTree = originalWalk })

	_, err = backend.Inventory(context.Background(), "")
	require.ErrorIs(t, err, fs.ErrNotExist)
	_, err = backend.NamespaceEmpty(context.Background())
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestFilesystemWalkTreatsInitiallyMissingRootAsEmpty(t *testing.T) {
	layout, err := NewLayout(filepath.Join(t.TempDir(), "missing"), LayoutOptions{
		Staging: StagingStoreDirectory, StagingDir: "tmp",
	})
	require.NoError(t, err)
	backend, err := NewFilesystemBackend(layout, FilesystemBackendOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })

	page, err := backend.Inventory(context.Background(), "")
	require.NoError(t, err)
	empty, err := backend.NamespaceEmpty(context.Background())
	require.NoError(t, err)

	assert.Empty(t, page.Objects)
	assert.Empty(t, page.Unknown)
	assert.True(t, empty)
}

func TestFilesystemBackendSeekablePackHonorsCancellation(t *testing.T) {
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	packPath, packID, entries := buildBackendPackSource(
		t, bytes.Repeat([]byte("cancel seekable pack"), 128<<10),
	)
	source, err := os.Open(packPath)
	require.NoError(t, err)
	_, err = backend.PublishPack(context.Background(), packID, source, PublishOptions{})
	require.NoError(t, errors.Join(err, source.Close()))
	require.Len(t, entries, 1)
	indexed, err := indexEntryFromPack(entries[0], packID)
	require.NoError(t, err)
	store, err := NewMultiStore(
		staticLocationResolver{resolution: Resolution{
			Member: true,
			Candidates: []ReadLocation{{
				StoreID: "archive", Generation: "epoch-1", Pack: &indexed,
			}},
		}},
		staticBackendRegistry{"archive": backend},
		MultiStoreOptions{},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	reader, _, err := store.Open(ctx, indexed.Hash)
	if reader != nil {
		_ = reader.Close()
	}

	require.ErrorIs(t, err, context.Canceled)
}

func TestFilesystemBackendDurablePackPublicationSyncsFreshHierarchy(t *testing.T) {
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	packPath, packID, _ := buildBackendPackSource(t, []byte("durable pack hierarchy"))
	originalSyncDir := pack.SyncDir
	var synced []string
	pack.SyncDir = func(dir string) error {
		synced = append(synced, filepath.Clean(dir))
		return nil
	}
	t.Cleanup(func() { pack.SyncDir = originalSyncDir })
	source, err := os.Open(packPath)
	require.NoError(t, err)

	_, err = backend.PublishPack(
		context.Background(), packID, source,
		PublishOptions{Durability: DurablePublication},
	)
	require.NoError(t, errors.Join(err, source.Close()))

	packsDir := backend.Layout().PacksDir()
	assert.Equal(t, []string{
		backend.Layout().Root(),
		packsDir,
		packsDir,
		filepath.Join(packsDir, packID[:2]),
	}, synced)
}

func TestFilesystemBackendRejectsDifferentPackAtExistingIdentity(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	firstPath, firstID, _ := buildBackendPackSource(t, []byte("first pack content"))
	first, err := os.Open(firstPath)
	require.NoError(err)
	_, err = backend.PublishPack(ctx, firstID, first, PublishOptions{})
	require.NoError(errors.Join(err, first.Close()))

	secondPath, _, _ := buildBackendPackSource(t, []byte("different pack content"))
	second, err := os.Open(secondPath)
	require.NoError(err)
	_, err = backend.PublishPack(ctx, firstID, second, PublishOptions{})
	require.ErrorIs(errors.Join(err, second.Close()), ErrContentMismatch)
}

func TestFilesystemBackendPublishPackRejectsDecoderWindow(t *testing.T) {
	content := bytes.Repeat([]byte("filesystem window policy "), 1<<15)
	packPath, packID := buildEncodedBackendPackSource(
		t,
		content,
		uint64(len(content)),
		8<<20,
	)
	limits := DefaultLimits()
	limits.BlobBytes = 2 << 20
	layout := layoutForStoreTest(t)
	backend, err := NewFilesystemBackend(layout, FilesystemBackendOptions{
		Limits: limits,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	owner := Ownership{
		Format: OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	require.NoError(t, backend.ReplaceOwnership(context.Background(), owner, nil))
	source, err := os.Open(packPath)
	require.NoError(t, err)

	_, err = backend.PublishPack(
		context.Background(),
		packID,
		source,
		PublishOptions{},
	)
	require.NoError(t, source.Close())

	require.ErrorIs(t, err, ErrBlobTooLarge)
	var limit *LimitError
	require.ErrorAs(t, err, &limit)
	assert.Equal(t, LimitBlobWindowBytes, limit.Dimension)
}

func TestFilesystemBackendPublishPackRejectsDecodedLengthMismatch(t *testing.T) {
	content := bytes.Repeat([]byte("decoded length authority "), 4096)
	tests := []struct {
		name   string
		rawLen uint64
	}{
		{name: "decoded content is longer", rawLen: uint64(len(content) - 1)},
		{name: "decoded content is shorter", rawLen: uint64(len(content) + 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			packPath, packID := buildEncodedBackendPackSource(
				t,
				content,
				tt.rawLen,
				0,
			)
			backend := attachedFilesystemBackend(t, "archive", "epoch-1")
			source, err := os.Open(packPath)
			require.NoError(t, err)

			_, err = backend.PublishPack(
				context.Background(),
				packID,
				source,
				PublishOptions{},
			)
			require.NoError(t, source.Close())

			require.ErrorIs(t, err, pack.ErrCorrupt)
			require.ErrorIs(t, err, ErrPhysicalCorrupt)
		})
	}
}

func TestFilesystemBackendPublishPackRejectsKnownConfiguredLimitBeforeRead(t *testing.T) {
	limits := DefaultLimits()
	limits.PackBytes = 8
	layout := layoutForStoreTest(t)
	backend, err := NewFilesystemBackend(layout, FilesystemBackendOptions{
		Limits: limits,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	owner := Ownership{
		Format: OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	require.NoError(t, backend.ReplaceOwnership(context.Background(), owner, nil))
	packID := pack.NewPackID()
	source := &countingPackReader{
		reader: bytes.NewReader(bytes.Repeat([]byte("x"), 9)),
	}

	_, err = backend.PublishPack(
		context.Background(),
		packID,
		source,
		PublishOptions{ExpectedSize: 9, SizeKnown: true, MaxBytes: 100},
	)

	require.ErrorIs(t, err, ErrBlobTooLarge)
	var limit *LimitError
	require.ErrorAs(t, err, &limit)
	assert.Equal(t, LimitPackContainerBytes, limit.Dimension)
	assert.Zero(t, source.reads)
	assert.NoFileExists(t, layout.PackPath(packID))
}

func TestFilesystemBackendPublishPackCapsCallerLimitBeforeCanonicalWrite(t *testing.T) {
	limits := DefaultLimits()
	limits.PackBytes = 8
	layout := layoutForStoreTest(t)
	backend, err := NewFilesystemBackend(layout, FilesystemBackendOptions{
		Limits: limits,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	owner := Ownership{
		Format: OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	require.NoError(t, backend.ReplaceOwnership(context.Background(), owner, nil))
	packID := pack.NewPackID()

	_, err = backend.PublishPack(
		context.Background(),
		packID,
		bytes.NewReader(bytes.Repeat([]byte("x"), 9)),
		PublishOptions{MaxBytes: 100},
	)

	require.ErrorIs(t, err, ErrBlobTooLarge)
	var limit *LimitError
	require.ErrorAs(t, err, &limit)
	assert.Equal(t, LimitPackContainerBytes, limit.Dimension)
	assert.NoFileExists(t, layout.PackPath(packID))
}

func TestFilesystemBackendPublishPackRejectsExactSizeMismatchBeforeCanonicalWrite(t *testing.T) {
	packPath, packID, _ := buildBackendPackSource(t, []byte("exact size publication"))
	info, err := os.Stat(packPath)
	require.NoError(t, err)
	for _, tt := range []struct {
		name         string
		expectedSize int64
	}{
		{name: "source is short", expectedSize: info.Size() + 1},
		{name: "source is overlong", expectedSize: info.Size() - 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := attachedFilesystemBackend(t, "archive", "epoch-1")
			source, err := os.Open(packPath)
			require.NoError(t, err)

			_, err = backend.PublishPack(
				context.Background(),
				packID,
				source,
				PublishOptions{ExpectedSize: tt.expectedSize, SizeKnown: true},
			)
			require.NoError(t, source.Close())

			require.ErrorIs(t, err, ErrContentMismatch)
			assert.NoFileExists(t, backend.Layout().PackPath(packID))
		})
	}
}

func TestFilesystemBackendPublishPackRejectsMalformedBeforeCanonicalWrite(t *testing.T) {
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	packID := pack.NewPackID()

	_, err := backend.PublishPack(
		context.Background(),
		packID,
		bytes.NewReader([]byte("not a pack")),
		PublishOptions{},
	)

	require.ErrorIs(t, err, pack.ErrBadMagic)
	require.ErrorIs(t, err, ErrPhysicalCorrupt)
	assert.NoFileExists(t, backend.Layout().PackPath(packID))
}

func TestFilesystemBackendPublishPackRejectsForgedBlobLimitBeforeCanonicalWrite(t *testing.T) {
	limits := DefaultLimits()
	limits.BlobBytes = 16
	packPath, packID := buildEncodedBackendPackSource(t, []byte("x"), 17, 0)
	layout := layoutForStoreTest(t)
	backend, err := NewFilesystemBackend(layout, FilesystemBackendOptions{Limits: limits})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	owner := Ownership{
		Format: OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	require.NoError(t, backend.ReplaceOwnership(context.Background(), owner, nil))
	source, err := os.Open(packPath)
	require.NoError(t, err)

	_, err = backend.PublishPack(context.Background(), packID, source, PublishOptions{})
	require.NoError(t, source.Close())

	require.ErrorIs(t, err, ErrBlobTooLarge)
	var limit *LimitError
	require.ErrorAs(t, err, &limit)
	assert.Equal(t, LimitBlobRawBytes, limit.Dimension)
	assert.NoFileExists(t, layout.PackPath(packID))
}

func TestFilesystemBackendPublishPackEnforcesZeroBlobLimit(t *testing.T) {
	tests := []struct {
		name      string
		content   []byte
		wantLimit bool
	}{
		{name: "nonempty blob", content: []byte("x"), wantLimit: true},
		{name: "empty blob", content: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limits := DefaultLimits()
			limits.BlobBytes = 0
			packPath, packID, _ := buildBackendPackSource(t, tt.content)
			layout := layoutForStoreTest(t)
			backend, err := NewFilesystemBackend(layout, FilesystemBackendOptions{Limits: limits})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, backend.Close()) })
			owner := Ownership{
				Format: OwnershipFormatV1,
				Vault:  "test-vault",
				Store:  "archive",
				Epoch:  "epoch-1",
			}
			require.NoError(t, backend.ReplaceOwnership(context.Background(), owner, nil))
			source, err := os.Open(packPath)
			require.NoError(t, err)

			_, err = backend.PublishPack(context.Background(), packID, source, PublishOptions{})
			require.NoError(t, source.Close())

			if !tt.wantLimit {
				require.NoError(t, err)
				assert.FileExists(t, layout.PackPath(packID))
				return
			}
			require.ErrorIs(t, err, ErrBlobTooLarge)
			var limit *LimitError
			require.ErrorAs(t, err, &limit)
			assert.Equal(t, LimitBlobRawBytes, limit.Dimension)
			assert.Equal(t, uint64(1), limit.Actual)
			assert.Zero(t, limit.Limit)
			assert.NoFileExists(t, layout.PackPath(packID))
		})
	}
}

func TestFilesystemBackendPublishPackPreflightsAllEntryLimitsBeforeIntegrity(t *testing.T) {
	staging := t.TempDir()
	writer, err := pack.NewWriter(staging, pack.WriterOptions{})
	require.NoError(t, err)
	_, err = writer.Append(nil)
	require.NoError(t, err)
	_, err = writer.Append([]byte("x"))
	require.NoError(t, err)
	packID := writer.ID()
	packPath := filepath.Join(staging, packID+PackExt)
	_, err = writer.Seal(packPath)
	require.NoError(t, err)
	packBytes, err := os.ReadFile(packPath)
	require.NoError(t, err)
	trailerOffset := len(packBytes) - plainPackTrailerSize
	footerLen := int(binary.LittleEndian.Uint32(packBytes[trailerOffset:]))
	footerOffset := trailerOffset - footerLen
	forgedID := pack.ComputeBlobID([]byte("not the empty blob"))
	copy(packBytes[footerOffset+4:footerOffset+4+len(forgedID)], forgedID[:])
	footerDigest := sha256.New()
	_, _ = footerDigest.Write(packBytes[footerOffset:trailerOffset])
	_, _ = footerDigest.Write(packBytes[trailerOffset : trailerOffset+4])
	copy(packBytes[trailerOffset+4:trailerOffset+36], footerDigest.Sum(nil))
	require.NoError(t, os.WriteFile(packPath, packBytes, 0o600))

	limits := DefaultLimits()
	limits.BlobBytes = 0
	layout := layoutForStoreTest(t)
	backend, err := NewFilesystemBackend(layout, FilesystemBackendOptions{Limits: limits})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	owner := Ownership{
		Format: OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	require.NoError(t, backend.ReplaceOwnership(context.Background(), owner, nil))
	source, err := os.Open(packPath)
	require.NoError(t, err)

	_, err = backend.PublishPack(context.Background(), packID, source, PublishOptions{})
	require.NoError(t, source.Close())

	require.ErrorIs(t, err, ErrBlobTooLarge)
	require.NotErrorIs(t, err, pack.ErrBlobMismatch)
	var limit *LimitError
	require.ErrorAs(t, err, &limit)
	assert.Equal(t, LimitBlobRawBytes, limit.Dimension)
	assert.Equal(t, uint64(1), limit.Actual)
	assert.Zero(t, limit.Limit)
	assert.NoFileExists(t, layout.PackPath(packID))
}

func TestCopyBoundedContextAcceptsMaxInt64Limit(t *testing.T) {
	var destination bytes.Buffer

	written, err := copyBoundedContext(
		context.Background(),
		&destination,
		bytes.NewReader([]byte("bounded content")),
		math.MaxInt64,
	)

	require.NoError(t, err)
	assert.Equal(t, int64(len("bounded content")), written)
	assert.Equal(t, "bounded content", destination.String())
}

func TestFilesystemBackendUsesAndRetiresExactLooseRepresentation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	content := bytes.Repeat([]byte("compressible authority "), 4096)
	hash := hashForTest(content)
	compressed, err := backend.PublishLoose(
		ctx,
		hash,
		bytes.NewReader(content),
		PublishOptions{
			ExpectedSize: int64(len(content)),
			SizeKnown:    true,
			Compression: LooseCompressionOptions{
				Enabled: true,
			},
		},
	)
	require.NoError(err)
	require.Equal(LooseEncodingZstd, compressed.Location.Encoding)
	rawPath := backend.Layout().LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(rawPath), 0o700))
	require.NoError(os.WriteFile(rawPath, content, 0o600))
	raw := LooseLocation{
		Encoding: LooseEncodingRaw, LogicalSize: int64(len(content)),
		StoredSize: int64(len(content)),
	}

	stream, _, err := backend.OpenLoose(ctx, hash, raw)
	require.NoError(err)
	got, err := io.ReadAll(stream)
	require.NoError(err)
	require.NoError(stream.Close())
	assert.Equal(content, got)

	require.NoError(backend.Retire(ctx, ObjectRef{
		LooseHash: hash, LooseEncoding: LooseEncodingRaw,
	}))
	assert.NoFileExists(rawPath)
	assert.FileExists(backend.Layout().CompressedLoosePath(hash))
	_, _, err = backend.OpenLoose(ctx, hash, raw)
	require.ErrorIs(err, ErrPhysicalMissing)
	stream, _, err = backend.OpenLoose(ctx, hash, compressed.Location)
	require.NoError(err)
	require.NoError(stream.Verify())
	require.NoError(stream.Close())
}

func TestFilesystemBackendRejectsAmbiguousLooseLocation(t *testing.T) {
	ctx := context.Background()
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	content := []byte("explicit loose representation required")
	hash := hashForTest(content)
	_, err := backend.PublishLoose(
		ctx,
		hash,
		bytes.NewReader(content),
		PublishOptions{ExpectedSize: int64(len(content)), SizeKnown: true},
	)
	require.NoError(t, err)

	stream, _, err := backend.OpenLoose(ctx, hash, LooseLocation{})
	if stream != nil {
		t.Cleanup(func() { _ = stream.Close() })
	}

	require.ErrorIs(t, err, ErrInvalidPolicy)
}

func TestFilesystemBackendRepairLooseOverwritesCorruptCanonical(t *testing.T) {
	ctx := context.Background()
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	content := []byte("trusted repair content")
	hash := hashForTest(content)
	published, err := backend.PublishLoose(
		ctx, hash, bytes.NewReader(content),
		PublishOptions{ExpectedSize: int64(len(content)), SizeKnown: true},
	)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		backend.Layout().LoosePath(hash), []byte("corrupt"), 0o600,
	))

	repaired, err := backend.RepairLoose(
		ctx, hash, bytes.NewReader(content),
		PublishOptions{ExpectedSize: int64(len(content)), SizeKnown: true},
	)
	require.NoError(t, err)
	assert.NotEqual(t, published.Generation, repaired.Generation)
	assert.False(t, repaired.Created)
	stream, _, err := backend.OpenLoose(ctx, hash, repaired.Location)
	require.NoError(t, err)
	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	assert.Equal(t, content, got)
}

func TestFilesystemBackendClassifiesTerminalStreamIntegrityErrors(t *testing.T) {
	tests := []struct {
		name   string
		stream *terminalErrorStream
		invoke func(VerifiedReadCloser) error
	}{
		{
			name:   "read content mismatch",
			stream: &terminalErrorStream{readErr: ErrContentMismatch},
			invoke: func(stream VerifiedReadCloser) error {
				_, err := stream.Read(make([]byte, 1))
				return err
			},
		},
		{
			name:   "verify corrupt pack",
			stream: &terminalErrorStream{verifyErr: pack.ErrCorrupt},
			invoke: func(stream VerifiedReadCloser) error {
				return stream.Verify()
			},
		},
		{
			name:   "close content mismatch",
			stream: &terminalErrorStream{closeErr: ErrContentMismatch},
			invoke: func(stream VerifiedReadCloser) error {
				return stream.Close()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.invoke(&physicalVerifiedStream{stream: tt.stream})
			require.ErrorIs(t, err, ErrPhysicalCorrupt)
		})
	}
}

func TestFilesystemBackendDoesNotClassifyIncompleteStreamCloseAsCorrupt(t *testing.T) {
	stream := &physicalVerifiedStream{stream: &terminalErrorStream{
		closeErr: pack.ErrVerificationIncomplete,
	}}

	err := stream.Close()

	require.ErrorIs(t, err, pack.ErrVerificationIncomplete)
	require.NotErrorIs(t, err, ErrPhysicalCorrupt)
}

func TestFilesystemBackendPreservesClosedStreamLifecycleErrors(t *testing.T) {
	ctx := context.Background()
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	content := []byte("early closed physical stream")
	hash := hashForTest(content)
	receipt, err := backend.PublishLoose(
		ctx,
		hash,
		bytes.NewReader(content),
		PublishOptions{ExpectedSize: int64(len(content)), SizeKnown: true},
	)
	require.NoError(t, err)
	stream, _, err := backend.OpenLoose(ctx, hash, receipt.Location)
	require.NoError(t, err)
	require.ErrorIs(t, stream.Close(), pack.ErrVerificationIncomplete)

	_, readErr := stream.Read(make([]byte, 1))
	verifyErr := stream.Verify()
	for _, err := range []error{readErr, verifyErr} {
		require.ErrorIs(t, err, os.ErrClosed)
		require.NotErrorIs(t, err, ErrStoreUnavailable)
		require.NotErrorIs(t, err, ErrPhysicalCorrupt)
	}
}

func TestMultiStoreFallsBackFromUnavailableFilesystemLooseObject(t *testing.T) {
	ctx := context.Background()
	content := []byte("healthy filesystem fallback")
	hash := hashForTest(content)
	primary := attachedFilesystemBackend(t, "primary", "primary-1")
	secondary := attachedFilesystemBackend(t, "secondary", "secondary-1")
	openErr := errors.New("filesystem device unavailable")
	primary.reader.openLooseFile = func(string) (*os.File, os.FileInfo, error) {
		return nil, nil, openErr
	}
	receipt, err := secondary.PublishLoose(
		ctx,
		hash,
		bytes.NewReader(content),
		PublishOptions{ExpectedSize: int64(len(content)), SizeKnown: true},
	)
	require.NoError(t, err)

	operations := []struct {
		name string
		read func(*testing.T, *Store) []byte
	}{
		{
			name: "OpenStream",
			read: func(t *testing.T, store *Store) []byte {
				t.Helper()
				stream, size, err := store.OpenStream(ctx, hash)
				require.NoError(t, err)
				require.Equal(t, int64(len(content)), size)
				data, err := io.ReadAll(stream)
				require.NoError(t, errors.Join(err, stream.Close()))
				return data
			},
		},
		{
			name: "Open",
			read: func(t *testing.T, store *Store) []byte {
				t.Helper()
				reader, size, err := store.Open(ctx, hash)
				require.NoError(t, err)
				require.Equal(t, int64(len(content)), size)
				data, err := io.ReadAll(reader)
				require.NoError(t, errors.Join(err, reader.Close()))
				return data
			},
		},
		{
			name: "ReadBounded",
			read: func(t *testing.T, store *Store) []byte {
				t.Helper()
				data, size, err := store.ReadBounded(ctx, hash, int64(len(content)))
				require.NoError(t, err)
				require.Equal(t, int64(len(content)), size)
				return data
			},
		},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			store, err := NewMultiStore(
				staticLocationResolver{resolution: Resolution{
					Member: true,
					Candidates: []ReadLocation{
						{StoreID: "primary", Generation: "primary-1", Loose: &receipt.Location},
						{StoreID: "secondary", Generation: "secondary-1", Loose: &receipt.Location},
					},
				}},
				staticBackendRegistry{"primary": primary, "secondary": secondary},
				MultiStoreOptions{},
			)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })

			assert.Equal(t, content, operation.read(t, store))
			_, _, err = primary.OpenLoose(ctx, hash, receipt.Location)
			require.ErrorIs(t, err, ErrStoreUnavailable)
			require.NotErrorIs(t, err, ErrPhysicalMissing)
			require.NotErrorIs(t, err, ErrPhysicalCorrupt)
			require.ErrorIs(t, err, openErr)
		})
	}
}

func TestMultiStoreFallsBackFromPackFooterCorruption(t *testing.T) {
	ctx := context.Background()
	content := []byte("healthy fallback pack content")
	hash := hashForTest(content)
	operations := []struct {
		name         string
		read         func(*testing.T, *Store, Hash) []byte
		primaryError func(*FilesystemBackend, IndexEntry) error
	}{
		{
			name: "OpenStream",
			read: func(t *testing.T, store *Store, hash Hash) []byte {
				t.Helper()
				stream, size, err := store.OpenStream(ctx, hash)
				require.NoError(t, err)
				require.Equal(t, int64(len(content)), size)
				data, err := io.ReadAll(stream)
				require.NoError(t, errors.Join(err, stream.Close()))
				return data
			},
			primaryError: func(backend *FilesystemBackend, entry IndexEntry) error {
				stream, _, err := backend.OpenPack(ctx, hash, entry)
				if stream != nil {
					return errors.Join(err, stream.Close())
				}
				return err
			},
		},
		{
			name: "Open",
			read: func(t *testing.T, store *Store, hash Hash) []byte {
				t.Helper()
				reader, size, err := store.Open(ctx, hash)
				require.NoError(t, err)
				require.Equal(t, int64(len(content)), size)
				data, err := io.ReadAll(reader)
				require.NoError(t, errors.Join(err, reader.Close()))
				return data
			},
			primaryError: func(backend *FilesystemBackend, entry IndexEntry) error {
				reader, _, err := backend.OpenSeekablePack(ctx, hash, entry)
				if reader != nil {
					return errors.Join(err, reader.Close())
				}
				return err
			},
		},
		{
			name: "ReadBounded",
			read: func(t *testing.T, store *Store, hash Hash) []byte {
				t.Helper()
				data, size, err := store.ReadBounded(ctx, hash, int64(len(content)))
				require.NoError(t, err)
				require.Equal(t, int64(len(content)), size)
				return data
			},
			primaryError: func(backend *FilesystemBackend, entry IndexEntry) error {
				_, _, err := backend.ReadPackBounded(ctx, hash, entry, int64(len(content)))
				return err
			},
		},
	}
	conditions := []struct {
		name        string
		primaryData []byte
		entry       func(IndexEntry) IndexEntry
	}{
		{
			name:        "missing footer entry",
			primaryData: []byte("different primary pack content"),
			entry: func(entry IndexEntry) IndexEntry {
				entry.Hash = hash
				return entry
			},
		},
		{
			name:        "catalog footer metadata mismatch",
			primaryData: content,
			entry: func(entry IndexEntry) IndexEntry {
				entry.Offset++
				return entry
			},
		},
	}

	for _, condition := range conditions {
		t.Run(condition.name, func(t *testing.T) {
			primary := attachedFilesystemBackend(t, "primary", "primary-1")
			secondary := attachedFilesystemBackend(t, "secondary", "secondary-1")
			primaryPath, primaryID, primaryEntries := buildBackendPackSource(t, condition.primaryData)
			primarySource, err := os.Open(primaryPath)
			require.NoError(t, err)
			_, err = primary.PublishPack(ctx, primaryID, primarySource, PublishOptions{})
			require.NoError(t, errors.Join(err, primarySource.Close()))
			primaryEntry, err := indexEntryFromPack(primaryEntries[0], primaryID)
			require.NoError(t, err)
			primaryEntry = condition.entry(primaryEntry)

			secondaryPath, secondaryID, secondaryEntries := buildBackendPackSource(t, content)
			secondarySource, err := os.Open(secondaryPath)
			require.NoError(t, err)
			_, err = secondary.PublishPack(ctx, secondaryID, secondarySource, PublishOptions{})
			require.NoError(t, errors.Join(err, secondarySource.Close()))
			secondaryEntry, err := indexEntryFromPack(secondaryEntries[0], secondaryID)
			require.NoError(t, err)

			for _, operation := range operations {
				t.Run(operation.name, func(t *testing.T) {
					store, err := NewMultiStore(
						staticLocationResolver{resolution: Resolution{
							Member: true,
							Candidates: []ReadLocation{
								{StoreID: "primary", Generation: "primary-1", Pack: &primaryEntry},
								{StoreID: "secondary", Generation: "secondary-1", Pack: &secondaryEntry},
							},
						}},
						staticBackendRegistry{"primary": primary, "secondary": secondary},
						MultiStoreOptions{},
					)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, store.Close()) })

					assert.Equal(t, content, operation.read(t, store, hash))
					err = operation.primaryError(primary, primaryEntry)
					require.ErrorIs(t, err, ErrPhysicalCorrupt)
					require.NotErrorIs(t, err, ErrPhysicalMissing)
				})
			}
		})
	}
}

func TestMultiStoreFallsBackFromFilesystemPackRepresentationLimits(t *testing.T) {
	ctx := context.Background()
	content := []byte("healthy filesystem representation fallback")
	tests := []struct {
		name      string
		dimension LimitDimension
		limit     func(Limits, int64) Limits
	}{
		{
			name:      "container bytes",
			dimension: LimitPackContainerBytes,
			limit: func(limits Limits, packSize int64) Limits {
				limits.PackBytes = packSize - 1
				return limits
			},
		},
		{
			name:      "footer bytes",
			dimension: LimitPackFooterBytes,
			limit: func(limits Limits, _ int64) Limits {
				limits.FooterBytes = 1
				return limits
			},
		},
		{
			name:      "entry count",
			dimension: LimitPackEntryCount,
			limit: func(limits Limits, _ int64) Limits {
				limits.PackEntries = 1
				return limits
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			published := attachedFilesystemBackend(t, "primary", "primary-1")
			packPath, packID, entries := buildBackendPackSource(
				t, content, []byte("second footer entry"),
			)
			packSource, err := os.Open(packPath)
			require.NoError(t, err)
			_, err = published.PublishPack(ctx, packID, packSource, PublishOptions{})
			require.NoError(t, errors.Join(err, packSource.Close()))
			info, err := os.Stat(packPath)
			require.NoError(t, err)
			indexed, err := indexEntryFromPack(entries[0], packID)
			require.NoError(t, err)

			limits := tt.limit(DefaultLimits(), info.Size())
			primary, err := NewFilesystemBackend(
				published.Layout(),
				FilesystemBackendOptions{Limits: limits},
			)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, primary.Close()) })
			healthy := &recordingReadBackend{content: content}
			store, err := NewMultiStore(
				staticLocationResolver{resolution: Resolution{
					Member: true,
					Candidates: []ReadLocation{
						{StoreID: "primary", Generation: "primary-1", Pack: &indexed},
						{StoreID: "healthy", Generation: "healthy-1", Loose: rawLocation(content)},
					},
				}},
				staticBackendRegistry{"primary": primary, "healthy": healthy},
				MultiStoreOptions{},
			)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })

			stream, size, err := store.OpenStream(ctx, indexed.Hash)
			require.NoError(t, err)
			got, err := io.ReadAll(stream)
			require.NoError(t, errors.Join(err, stream.Close()))
			assert.Equal(t, content, got)
			assert.Equal(t, int64(len(content)), size)

			_, _, err = primary.OpenPack(ctx, indexed.Hash, indexed)
			require.ErrorIs(t, err, ErrPhysicalCorrupt)
			require.ErrorIs(t, err, ErrBlobTooLarge)
			var limit *LimitError
			require.ErrorAs(t, err, &limit)
			assert.Equal(t, tt.dimension, limit.Dimension)
		})
	}
}

func TestFilesystemBackendClassifiesLateLooseIntegrityFailure(t *testing.T) {
	ctx := context.Background()
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	content := []byte("trusted loose content")
	hash := hashForTest(content)
	receipt, err := backend.PublishLoose(
		ctx,
		hash,
		bytes.NewReader(content),
		PublishOptions{ExpectedSize: int64(len(content)), SizeKnown: true},
	)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		backend.Layout().LoosePath(hash),
		bytes.Repeat([]byte("x"), len(content)),
		0o600,
	))

	stream, _, err := backend.OpenLoose(ctx, hash, receipt.Location)
	require.NoError(t, err)
	err = stream.Verify()

	require.ErrorIs(t, err, ErrPhysicalCorrupt)
	require.ErrorIs(t, stream.Close(), ErrPhysicalCorrupt)
}

func TestFilesystemOwnershipRejectsNoncanonicalMarker(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	noncanonical := []byte(`{"epoch":"epoch-1","store":"archive","vault":"test-vault","format":1}` + "\n")
	require.NoError(os.WriteFile(backend.Layout().OwnershipPath(), noncanonical, 0o600))

	_, err := backend.Ownership(ctx)

	require.ErrorContains(err, "not canonical")
	_, err = backend.PublishLoose(
		ctx,
		hashForTest([]byte("blocked")),
		bytes.NewReader([]byte("blocked")),
		PublishOptions{ExpectedSize: 7, SizeKnown: true},
	)
	require.ErrorIs(err, ErrStoreFenced)
}

type terminalErrorStream struct {
	readErr   error
	verifyErr error
	closeErr  error
}

func (s *terminalErrorStream) Read([]byte) (int, error) { return 0, s.readErr }
func (s *terminalErrorStream) Verify() error            { return s.verifyErr }
func (s *terminalErrorStream) Verified() bool           { return false }
func (s *terminalErrorStream) Close() error             { return s.closeErr }

func buildBackendPackSource(
	t *testing.T,
	contents ...[]byte,
) (string, string, []pack.Entry) {
	t.Helper()
	root := t.TempDir()
	writer, err := pack.NewWriter(root, pack.WriterOptions{})
	require.NoError(t, err)
	for _, content := range contents {
		_, err = writer.Append(content)
		require.NoError(t, err)
	}
	packID := writer.ID()
	path := filepath.Join(root, packID+PackExt)
	entries, err := writer.Seal(path)
	require.NoError(t, err)
	return path, packID, entries
}

func buildEncodedBackendPackSource(
	t *testing.T,
	content []byte,
	rawLen uint64,
	windowBytes int,
) (string, string) {
	t.Helper()
	var frame bytes.Buffer
	options := []zstd.EOption{zstd.WithEncoderConcurrency(1)}
	if windowBytes > 0 {
		options = append(options, zstd.WithWindowSize(windowBytes))
	}
	encoder, err := zstd.NewWriter(&frame, options...)
	require.NoError(t, err)
	_, err = encoder.Write(content)
	require.NoError(t, err)
	require.NoError(t, encoder.Close())
	staging := t.TempDir()
	writer, err := pack.NewWriter(staging, pack.WriterOptions{})
	require.NoError(t, err)
	_, err = writer.AppendEncoded(
		pack.ComputeBlobID(content),
		frame.Bytes(),
		rawLen,
		true,
	)
	require.NoError(t, err)
	packID := writer.ID()
	path := filepath.Join(staging, packID+PackExt)
	_, err = writer.Seal(path)
	require.NoError(t, err)
	return path, packID
}

type countingPackReader struct {
	reader io.Reader
	reads  int
}

func (r *countingPackReader) Read(p []byte) (int, error) {
	r.reads++
	return r.reader.Read(p)
}

func indexEntryFromPack(entry pack.Entry, packID string) (IndexEntry, error) {
	hash, err := ParseHash(entry.ID.String())
	if err != nil {
		return IndexEntry{}, err
	}
	return IndexEntry{
		Hash: hash, PackID: packID, Offset: int64(entry.Offset),
		StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen),
		Flags: uint8(entry.Flags), CRC32C: entry.CRC32C,
	}, nil
}
