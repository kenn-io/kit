package pack

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppendStreamRoundTrip(t *testing.T) {
	t.Parallel()
	incompressible := make([]byte, 64<<10)
	var state uint32 = 1
	for i := range incompressible {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		incompressible[i] = byte(state)
	}
	tests := []struct {
		name       string
		content    []byte
		compressed bool
	}{
		{name: "empty", content: []byte{}},
		{name: "raw", content: []byte("plain streamed content")},
		{name: "below-zstd-window", content: make([]byte, 1023)},
		{name: "at-zstd-window", content: make([]byte, 1024), compressed: true},
		{name: "incompressible", content: incompressible},
		{name: "compressed", content: bytes.Repeat([]byte("compressible-stream-"), 1<<16), compressed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writer, err := NewWriter(dir, WriterOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { _ = writer.Abort() })

			id := ComputeBlobID(tt.content)
			entry, err := writer.AppendStream(context.Background(), bytes.NewReader(tt.content), uint64(len(tt.content)), AppendStreamOptions{
				ExpectedID:   &id,
				ScratchDir:   dir,
				ScratchBytes: uint64(len(tt.content))*3 + 1024,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.compressed, entry.Flags&BlobCompressed != 0)
			assert.Equal(t, id, entry.ID)

			matches, err := filepath.Glob(filepath.Join(dir, "pack-prepared-*"))
			require.NoError(t, err)
			assert.Empty(t, matches)

			final := filepath.Join(dir, writer.ID()+".pack")
			_, err = writer.Seal(final)
			require.NoError(t, err)
			reader, err := OpenReader(final, nil)
			require.NoError(t, err)
			if tt.compressed {
				window, windowErr := reader.streamingWindow(reader.Entries()[0])
				require.NoError(t, windowErr)
				assert.LessOrEqual(t, window, uint64(streamMaxWindowSize))
			}
			stream, err := reader.OpenBlob(context.Background(), reader.Entries()[0])
			require.NoError(t, err)
			got, err := io.ReadAll(stream)
			require.NoError(t, err)
			assert.Equal(t, tt.content, got)
			assert.True(t, stream.Verified())
			require.NoError(t, stream.Verify())
			require.NoError(t, stream.Close())

			buffered, err := reader.ReadBlob(reader.Entries()[0])
			require.NoError(t, err)
			assert.Equal(t, tt.content, buffered)
			require.NoError(t, reader.Close())
		})
	}
}

func TestAppendStreamAboveLegacyCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a blob above the former 64 MiB policy ceiling")
	}
	const size = uint64(64<<20 + 1)
	tests := []struct {
		name       string
		source     func() io.Reader
		compressed bool
	}{
		{name: "compressed", source: func() io.Reader { return zeroReader{} }, compressed: true},
		{name: "raw", source: func() io.Reader { return &noiseReader{state: 1} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writer, err := NewWriter(dir, WriterOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { _ = writer.Abort() })
			source := io.LimitReader(tt.source(), int64(size))
			entry, err := writer.AppendStream(context.Background(), source, size, AppendStreamOptions{
				ScratchDir: dir, ScratchBytes: 140 << 20,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.compressed, entry.Flags&BlobCompressed != 0)
			final := filepath.Join(dir, writer.ID()+".pack")
			_, err = writer.Seal(final)
			require.NoError(t, err)

			reader, err := OpenReader(final, nil)
			require.NoError(t, err)
			stream, err := reader.OpenBlob(context.Background(), reader.Entries()[0])
			require.NoError(t, err)
			require.NoError(t, stream.Verify())
			assert.True(t, stream.Verified())
			require.NoError(t, stream.Close())
			require.NoError(t, reader.Close())
		})
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type noiseReader struct{ state uint32 }

func (r *noiseReader) Read(p []byte) (int, error) {
	for i := range p {
		r.state ^= r.state << 13
		r.state ^= r.state >> 17
		r.state ^= r.state << 5
		p[i] = byte(r.state)
	}
	return len(p), nil
}

func TestAppendStreamSourceFailuresLeaveWriterUsable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Abort() })

	_, err = writer.AppendStream(context.Background(), strings.NewReader("short"), 6, AppendStreamOptions{ScratchDir: dir})
	require.ErrorIs(t, err, ErrTruncated)
	_, err = writer.AppendStream(context.Background(), strings.NewReader("trailing"), 5, AppendStreamOptions{ScratchDir: dir})
	require.ErrorIs(t, err, ErrCorrupt)

	content := []byte("valid after source failures")
	entry, err := writer.AppendStream(context.Background(), bytes.NewReader(content), uint64(len(content)), AppendStreamOptions{ScratchDir: dir})
	require.NoError(t, err)
	assert.Equal(t, ComputeBlobID(content), entry.ID)
}

func TestPrepareBlobCancellationCleansScratch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	source := &cancelingReader{remaining: 1 << 20, cancel: cancel}
	_, err := PrepareBlob(ctx, source, 1<<20, DefaultZstdLevel, AppendStreamOptions{ScratchDir: dir})
	require.ErrorIs(t, err, context.Canceled)
	matches, err := filepath.Glob(filepath.Join(dir, "pack-prepared-*"))
	require.NoError(t, err)
	assert.Empty(t, matches)
}

func TestPrepareBlobPreservesSourceError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceErr := errors.New("source failed")
	_, err := PrepareBlob(context.Background(), &failingReader{err: sourceErr}, 1<<20, DefaultZstdLevel, AppendStreamOptions{ScratchDir: dir})
	require.ErrorIs(t, err, sourceErr)
	matches, err := filepath.Glob(filepath.Join(dir, "pack-prepared-*"))
	require.NoError(t, err)
	assert.Empty(t, matches)
}

type failingReader struct {
	wrote bool
	err   error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.wrote {
		return 0, r.err
	}
	r.wrote = true
	n := min(len(p), 128)
	clear(p[:n])
	return n, nil
}

type cancelingReader struct {
	remaining int
	cancel    context.CancelFunc
	didCancel bool
}

func (r *cancelingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	clear(p[:n])
	r.remaining -= n
	if !r.didCancel {
		r.didCancel = true
		r.cancel()
	}
	return n, nil
}

func TestPrepareBlobLimitsAndIdentity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := bytes.Repeat([]byte("x"), 1<<16)
	wrong := ComputeBlobID([]byte("wrong"))

	_, err := PrepareBlob(context.Background(), bytes.NewReader(content), uint64(len(content)), DefaultZstdLevel, AppendStreamOptions{
		ScratchDir: dir, ScratchBytes: uint64(len(content)),
	})
	var limitErr *StreamLimitError
	require.ErrorAs(t, err, &limitErr)
	assert.Equal(t, StreamLimitScratchBytes, limitErr.Dimension)

	_, err = PrepareBlob(context.Background(), bytes.NewReader(content), uint64(len(content)), DefaultZstdLevel, AppendStreamOptions{
		ExpectedID: &wrong, ScratchDir: dir,
	})
	require.ErrorIs(t, err, ErrBlobMismatch)
	matches, globErr := filepath.Glob(filepath.Join(dir, "pack-prepared-*"))
	require.NoError(t, globErr)
	assert.Empty(t, matches)

	_, err = PrepareBlob(context.Background(), strings.NewReader(""), MaxRawLen+1, DefaultZstdLevel, AppendStreamOptions{ScratchDir: dir})
	require.ErrorAs(t, err, &limitErr)
	assert.Equal(t, StreamLimitRawBytes, limitErr.Dimension)

	small := []byte("small")
	prepared, err := PrepareBlob(context.Background(), bytes.NewReader(small), uint64(len(small)), DefaultZstdLevel, AppendStreamOptions{
		ScratchDir: dir, ScratchBytes: uint64(len(small)),
	})
	require.NoError(t, err)
	require.NoError(t, prepared.Close())
}

func TestPreparedBlobCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := []byte("discard prepared content")
	prepared, err := PrepareBlob(context.Background(), bytes.NewReader(content), uint64(len(content)), DefaultZstdLevel, AppendStreamOptions{ScratchDir: dir})
	require.NoError(t, err)
	require.NoError(t, prepared.Close())
	require.NoError(t, prepared.Close())
	matches, err := filepath.Glob(filepath.Join(dir, "pack-prepared-*"))
	require.NoError(t, err)
	assert.Empty(t, matches)
}

func TestAppendPreparedZeroByteWriteFailureDoesNotPoisonWriter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := bytes.Repeat([]byte("prepared"), 1<<14)
	prepared, err := PrepareBlob(context.Background(), bytes.NewReader(content), uint64(len(content)), DefaultZstdLevel, AppendStreamOptions{ScratchDir: dir})
	require.NoError(t, err)

	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Abort() })
	require.NoError(t, writer.f.Close())
	_, err = writer.AppendPrepared(context.Background(), prepared)
	require.Error(t, err)
	assert.Nil(t, writer.err)
}

func TestAppendPreparedCancellationBeforeCopyLeavesWriterUsable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := bytes.Repeat([]byte("prepared cancellation"), 1<<12)
	prepared, err := PrepareBlob(context.Background(), bytes.NewReader(content), uint64(len(content)), DefaultZstdLevel, AppendStreamOptions{ScratchDir: dir})
	require.NoError(t, err)

	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Abort() })
	ctx, cancel := context.WithCancel(context.Background())
	cancelBetweenChecks := &cancelAfterFirstErrContext{Context: ctx, cancel: cancel}
	_, err = writer.AppendPrepared(cancelBetweenChecks, prepared)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int64(headerSize), writer.StoredSize())

	later := []byte("later append")
	entry, err := writer.Append(later)
	require.NoError(t, err)
	assert.Equal(t, ComputeBlobID(later), entry.ID)
}

type cancelAfterFirstErrContext struct {
	context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelAfterFirstErrContext) Err() error {
	err := c.Context.Err()
	c.once.Do(c.cancel)
	return err
}

func TestAppendPreparedScratchCorruptionPoisonsWriter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := bytes.Repeat([]byte("prepared corruption"), 1<<12)
	prepared, err := PrepareBlob(context.Background(), bytes.NewReader(content), uint64(len(content)), DefaultZstdLevel, AppendStreamOptions{ScratchDir: dir})
	require.NoError(t, err)
	_, err = prepared.f.WriteAt([]byte{0xff}, 0)
	require.NoError(t, err)

	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Abort() })
	_, firstErr := writer.AppendPrepared(context.Background(), prepared)
	require.ErrorIs(t, firstErr, ErrCorrupt)
	_, nextErr := writer.Append([]byte("later"))
	assert.EqualError(t, nextErr, firstErr.Error())
}

func TestBlobReaderTerminalVerificationAndParentLifetime(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := []byte("terminal verification content")
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(t, err)
	entry, err := writer.Append(content)
	require.NoError(t, err)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(t, err)

	reader, err := OpenReader(final, nil)
	require.NoError(t, err)
	stream, err := reader.OpenBlob(context.Background(), entry)
	require.NoError(t, err)
	require.ErrorIs(t, reader.Close(), ErrStreamsActive)
	buf := make([]byte, 4)
	_, err = stream.Read(buf)
	require.NoError(t, err)
	require.ErrorIs(t, stream.Close(), ErrVerificationIncomplete)
	require.NoError(t, reader.Close())
}

func TestBlobReaderRejectsEntryOutsideVerifiedFooter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secret := []byte("secret entry bytes")
	public := []byte("public entry bytes")
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(t, err)
	secretEntry, err := writer.Append(secret)
	require.NoError(t, err)
	publicEntry, err := writer.Append(public)
	require.NoError(t, err)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(t, err)

	reader, err := OpenReader(final, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	listed := reader.Entries()
	listed[0] = publicEntry
	assert.Equal(t, secretEntry, reader.Entries()[0], "returned footer entries must not mutate reader authority")

	forged := publicEntry
	forged.Offset = secretEntry.Offset
	forged.StoredLen = secretEntry.StoredLen
	forged.RawLen = secretEntry.RawLen
	stream, err := reader.OpenBlob(context.Background(), forged)
	require.ErrorIs(t, err, ErrCorrupt)
	require.ErrorContains(t, err, "does not match verified footer")
	assert.Nil(t, stream)
}

func TestBlobReaderCancellationIsTerminal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := bytes.Repeat([]byte("cancel stream"), 1<<14)
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(t, err)
	entry, err := writer.Append(content)
	require.NoError(t, err)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(t, err)
	reader, err := OpenReader(final, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := reader.OpenBlob(ctx, entry)
	require.NoError(t, err)
	buf := make([]byte, 32)
	_, err = stream.Read(buf)
	require.NoError(t, err)
	cancel()
	_, err = stream.Read(buf)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, stream.Verify(), context.Canceled)
	require.ErrorIs(t, stream.Close(), context.Canceled)
	require.NoError(t, reader.Close())
}

func TestBlobReaderReportsTerminalIntegrityErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := []byte("content delivered before terminal verification")
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(t, err)
	entry, err := writer.Append(content)
	require.NoError(t, err)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(t, err)

	f, err := os.OpenFile(final, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{'X'}, int64(entry.Offset))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	reader, err := OpenReader(final, nil)
	require.NoError(t, err)
	stream, err := reader.OpenBlob(context.Background(), reader.Entries()[0])
	require.NoError(t, err)
	got, err := io.ReadAll(stream)
	require.ErrorIs(t, err, ErrCorrupt)
	assert.Len(t, got, len(content))
	assert.False(t, stream.Verified())
	require.ErrorIs(t, stream.Close(), ErrVerificationIncomplete)
	require.NoError(t, reader.Close())
}

func TestBlobReaderReportsCompressedDecodeFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := bytes.Repeat([]byte("compressed corruption "), 1<<15)
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(t, err)
	entry, err := writer.AppendStream(context.Background(), bytes.NewReader(content), uint64(len(content)), AppendStreamOptions{ScratchDir: dir})
	require.NoError(t, err)
	require.NotZero(t, entry.Flags&BlobCompressed)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(t, err)

	f, err := os.OpenFile(final, os.O_RDWR, 0)
	require.NoError(t, err)
	corruptAt := int64(entry.Offset + entry.StoredLen/2) //nolint:gosec // test frame is small
	var original [1]byte
	_, err = f.ReadAt(original[:], corruptAt)
	require.NoError(t, err)
	original[0] ^= 0xff
	_, err = f.WriteAt(original[:], corruptAt)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	reader, err := OpenReader(final, nil)
	require.NoError(t, err)
	stream, err := reader.OpenBlob(context.Background(), reader.Entries()[0])
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, stream)
	require.ErrorIs(t, err, ErrCorrupt)
	require.ErrorIs(t, stream.Close(), ErrVerificationIncomplete)
	require.NoError(t, reader.Close())
}

func TestBlobReaderDetectsHashMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := []byte("hash checked at eof")
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(t, err)
	entry, err := writer.Append(content)
	require.NoError(t, err)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(t, err)
	forged := entry
	forged.ID = ComputeBlobID([]byte("different"))
	data, err := os.ReadFile(final)
	require.NoError(t, err)
	footerStart := int(entry.Offset + entry.StoredLen)
	rebuilt := append([]byte{}, data[:footerStart]...)
	rebuilt = append(rebuilt, appendPlainTrailer(encodeFooterRegion([]Entry{forged}))...)
	require.NoError(t, os.WriteFile(final, rebuilt, 0o600))

	reader, err := OpenReader(final, nil)
	require.NoError(t, err)
	stream, err := reader.OpenBlob(context.Background(), reader.Entries()[0])
	require.NoError(t, err)
	got, err := io.ReadAll(stream)
	assert.Equal(t, content, got)
	require.ErrorIs(t, err, ErrBlobMismatch)
	require.ErrorIs(t, stream.Verify(), ErrBlobMismatch)
	require.ErrorIs(t, stream.Close(), ErrBlobMismatch)
	_, repeatedErr := stream.Read(make([]byte, 1))
	require.EqualError(t, repeatedErr, err.Error())
	require.EqualError(t, stream.Verify(), err.Error())
	require.NoError(t, reader.Close())
}

func TestStreamingEncryptedV1IsUnsupported(t *testing.T) {
	t.Parallel()
	key := [32]byte{1}
	crypter, err := NewCrypter(key)
	require.NoError(t, err)
	dir := t.TempDir()
	writer, err := NewWriter(dir, WriterOptions{Crypter: crypter})
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Abort() })
	_, err = writer.AppendStream(context.Background(), strings.NewReader("secret"), 6, AppendStreamOptions{})
	require.ErrorIs(t, err, ErrStreamUnsupported)

	entry, err := writer.Append([]byte("buffered secret"))
	require.NoError(t, err)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(t, err)
	reader, err := OpenReader(final, crypter)
	require.NoError(t, err)
	_, err = reader.OpenBlob(context.Background(), entry)
	require.ErrorIs(t, err, ErrStreamUnsupported)
	require.NoError(t, reader.Close())
}

func TestOpenReaderWithOptionsEnforcesLimits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(t, err)
	_, err = writer.Append([]byte("bounded content"))
	require.NoError(t, err)
	_, err = writer.Append([]byte("second"))
	require.NoError(t, err)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(t, err)

	_, err = OpenReaderWithOptions(final, nil, ReaderOptions{Limits: ReaderLimits{RawBytes: 1}})
	var limitErr *StreamLimitError
	require.ErrorAs(t, err, &limitErr)
	assert.Equal(t, StreamLimitRawBytes, limitErr.Dimension)

	info, err := os.Stat(final)
	require.NoError(t, err)
	_, err = OpenReaderWithOptions(final, nil, ReaderOptions{Limits: ReaderLimits{ContainerBytes: uint64(info.Size() - 1)}}) //nolint:gosec
	require.ErrorAs(t, err, &limitErr)
	assert.Equal(t, StreamLimitContainerBytes, limitErr.Dimension)

	_, err = OpenReaderWithOptions(final, nil, ReaderOptions{Limits: ReaderLimits{Entries: 1}})
	require.ErrorAs(t, err, &limitErr)
	assert.Equal(t, StreamLimitEntryCount, limitErr.Dimension)

	_, err = OpenReaderWithOptions(final, nil, ReaderOptions{Limits: ReaderLimits{FooterBytes: 1}})
	require.ErrorAs(t, err, &limitErr)
	assert.Equal(t, StreamLimitFooterBytes, limitErr.Dimension)

	_, err = OpenReaderWithOptions(final, nil, ReaderOptions{Limits: ReaderLimits{StoredBytes: 1}})
	require.ErrorAs(t, err, &limitErr)
	assert.Equal(t, StreamLimitStoredBytes, limitErr.Dimension)
}

func TestBlobReaderEnforcesZstdWindowLimit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := bytes.Repeat([]byte("legacy-single-segment"), 1<<16)
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(t, err)
	entry, err := writer.Append(content)
	require.NoError(t, err)
	require.NotZero(t, entry.Flags&BlobCompressed)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(t, err)

	reader, err := OpenReaderWithOptions(final, nil, ReaderOptions{Limits: ReaderLimits{WindowBytes: 64 << 10}})
	require.NoError(t, err)
	_, err = reader.OpenBlob(context.Background(), reader.Entries()[0])
	var limitErr *StreamLimitError
	require.ErrorAs(t, err, &limitErr)
	assert.Equal(t, StreamLimitWindowBytes, limitErr.Dimension)
	assert.Greater(t, limitErr.Actual, limitErr.Limit)
	require.NoError(t, reader.Close())
}

func TestBlobReaderReadsFrozenV1Fixture(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "packstore", "testdata", "msgvault-v1", "01kx758hcw5gnkdz233217fd9a.mvpack")
	reader, err := OpenReader(path, nil)
	require.NoError(t, err)
	compressed := false
	for _, entry := range reader.Entries() {
		compressed = compressed || entry.Flags&BlobCompressed != 0
		stream, openErr := reader.OpenBlob(context.Background(), entry)
		require.NoError(t, openErr)
		require.NoError(t, stream.Verify())
		assert.True(t, stream.Verified())
		require.NoError(t, stream.Close())
	}
	assert.True(t, compressed)
	require.NoError(t, reader.Close())
}

func TestOpenBlobHonorsCancellation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(t, err)
	entry, err := writer.Append([]byte("cancelled"))
	require.NoError(t, err)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(t, err)
	reader, err := OpenReader(final, nil)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = reader.OpenBlob(ctx, entry)
	require.ErrorIs(t, err, context.Canceled)
}
