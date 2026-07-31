package packstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
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
	content []byte,
) (string, string, []pack.Entry) {
	t.Helper()
	root := t.TempDir()
	writer, err := pack.NewWriter(root, pack.WriterOptions{})
	require.NoError(t, err)
	_, err = writer.Append(content)
	require.NoError(t, err)
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
