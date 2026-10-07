//go:build !race

package pack

import (
	"crypto/sha256"
	"io"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests need real blobs over 512 MiB and a 64-bit address space. Run them
// in ordinary CI, but omit race builds where instrumentation multiplies their
// memory cost.
func TestEncodeFrameRoundTripsBlobsOverMaxWindow(t *testing.T) {
	if testing.Short() || strconv.IntSize == 32 {
		t.Skip("requires a 64-bit process and more than 512 MiB")
	}

	const size = zstd.MaxWindowSize + 1
	raw := make([]byte, size)
	for i := range raw {
		raw[i] = byte(i % 251)
	}
	wantHash := sha256.Sum256(raw)
	stored, compressed := EncodeFrame(raw, DefaultZstdLevel)
	require.True(t, compressed)

	var header zstd.Header
	require.NoError(t, header.Decode(stored))
	require.False(t, header.SingleSegment,
		"blobs over the window limit must not use single-segment framing")
	assert.LessOrEqual(t, header.WindowSize, uint64(zstd.MaxWindowSize))

	// Keep the previous reader's 512 MiB window limit. Reuse the input buffer
	// after encoding to avoid another allocation proportional to the blob.
	decoder, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(MaxRawLen))
	require.NoError(t, err)
	t.Cleanup(decoder.Close)
	decoded, err := decoder.DecodeAll(stored, raw[:0])
	require.NoError(t, err)
	assert.Len(t, decoded, size)
	assert.Equal(t, wantHash, sha256.Sum256(decoded))
}

func TestReaderReadsOversizedSingleSegmentFrame(t *testing.T) {
	if testing.Short() || strconv.IntSize == 32 {
		t.Skip("requires a 64-bit process and more than 512 MiB")
	}

	const size = zstd.MaxWindowSize + 1
	raw := make([]byte, size)
	for i := range raw {
		raw[i] = byte(i % 251)
	}
	wantHash := sha256.Sum256(raw)

	// Reproduce the old writer independently of EncodeFrame, which now avoids
	// single-segment framing for oversized blobs.
	encoder, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(DefaultZstdLevel)),
		zstd.WithEncoderConcurrency(1), zstd.WithSingleSegment(true))
	require.NoError(t, err)
	stored := encoder.EncodeAll(raw, nil)
	require.NoError(t, encoder.Close())
	var header zstd.Header
	require.NoError(t, header.Decode(stored))
	require.True(t, header.SingleSegment)
	require.Equal(t, uint64(size), header.FrameContentSize)

	dir := t.TempDir()
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, writer.Abort()) })
	_, err = writer.AppendEncoded(BlobID(wantHash), stored, size, true)
	require.NoError(t, err)
	path := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(path)
	require.NoError(t, err)
	reader, err := OpenReader(path, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, reader.Close()) })
	entry := reader.Entries()[0]

	t.Run("ReadBlob", func(t *testing.T) {
		decoded, err := reader.ReadBlob(entry)
		require.NoError(t, err)
		assert.Len(t, decoded, size)
		assert.Equal(t, wantHash, sha256.Sum256(decoded))
	})
	t.Run("OpenBlob", func(t *testing.T) {
		stream, err := reader.OpenBlob(t.Context(), entry)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, stream.Close()) })
		hash := sha256.New()
		n, err := io.Copy(hash, stream)
		require.NoError(t, err)
		assert.Equal(t, int64(size), n)
		assert.Equal(t, wantHash[:], hash.Sum(nil))
		assert.True(t, stream.Verified())
	})
}
