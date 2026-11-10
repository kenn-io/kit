package packstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"

	"github.com/klauspost/compress/zstd"
	"go.kenn.io/kit/internal/contextio"
	"go.kenn.io/kit/pack"
)

// maxCopiedFrameBytes bounds the stored frame rewriteSources holds in memory
// to copy it into the output pack without recompressing it. One rewrite holds
// one buffer, sized to the largest frame it copied, and the Maintainer runs
// one rewrite at a time, so 16 MiB stays far below what a 4-core, 8 GiB host
// can spare while covering every merge source a quarter-target
// MergeBelowStored selects. Larger frames stream through PrepareBlob's disk
// scratch, where recompression costs little next to the I/O. Tests lower it
// to exercise that path.
var maxCopiedFrameBytes = uint64(16 << 20)

// sourceFrames reads live entries' stored frames from the source pack each
// entry names, keeping one source open at a time. Callers pass entries
// grouped by source, so each source opens once per rewrite.
type sourceFrames struct {
	layout  Layout
	limits  Limits
	packID  string
	reader  *MaintenancePackReader
	frame   []byte
	copyBuf []byte
	decoder *zstd.Decoder
}

func newSourceFrames(layout Layout, limits Limits) *sourceFrames {
	return &sourceFrames{layout: layout, limits: limits}
}

// entry returns the authoritative footer entry for indexed, opening its source
// pack when it differs from the open one.
func (s *sourceFrames) entry(indexed IndexEntry) (pack.Entry, error) {
	if s.reader == nil || s.packID != indexed.PackID {
		s.closeReader()
		reader, err := OpenMaintenancePack(s.layout.PackPath(indexed.PackID), s.limits)
		if err != nil {
			return pack.Entry{}, err
		}
		s.reader, s.packID = reader, indexed.PackID
	}
	id, err := pack.ParseBlobID(indexed.Hash.String())
	if err != nil {
		return pack.Entry{}, err
	}
	footer, ok := s.reader.reader.entries[id]
	if !ok {
		return pack.Entry{}, fmt.Errorf("%w: pack footer has no entry for %s", ErrContentMismatch, indexed.Hash)
	}
	if !packIndexMatchesFooter(&indexed, footer) {
		return pack.Entry{}, fmt.Errorf("%w: pack index metadata mismatch for %s", ErrContentMismatch, indexed.Hash)
	}
	limit := uint64(s.limits.BlobBytes)
	if footer.RawLen > limit {
		return pack.Entry{}, newLimitError(LimitBlobRawBytes, footer.RawLen, limit)
	}
	if footer.StoredLen > limit {
		return pack.Entry{}, newLimitError(LimitBlobStoredBytes, footer.StoredLen, limit)
	}
	return footer, nil
}

// readVerified returns entry's stored frame after checking its CRC32C, then
// decoding it and checking its length and SHA-256. The decode streams into the
// hash, so only the stored frame is held in memory. The returned slice is
// reused by the next call.
func (s *sourceFrames) readVerified(ctx context.Context, entry pack.Entry) ([]byte, error) {
	s.frame = slices.Grow(s.frame[:0], int(entry.StoredLen))[:entry.StoredLen]
	if _, err := s.reader.reader.file.ReadAt(s.frame, int64(entry.Offset)); err != nil {
		return nil, fmt.Errorf("%w: read stored bytes for %s: %w", pack.ErrCorrupt, entry.ID, err)
	}
	if crc32.Checksum(s.frame, boundedCRC32CTable) != entry.CRC32C {
		return nil, fmt.Errorf("%w: crc mismatch for %s", pack.ErrCorrupt, entry.ID)
	}
	if entry.Flags&pack.BlobCompressed == 0 {
		if entry.StoredLen != entry.RawLen {
			return nil, fmt.Errorf("%w: raw frame length %d differs from decoded length %d",
				pack.ErrCorrupt, entry.StoredLen, entry.RawLen)
		}
		if sha256.Sum256(s.frame) != entry.ID {
			return nil, fmt.Errorf("%w: blob %s", pack.ErrBlobMismatch, entry.ID)
		}
		return s.frame, nil
	}
	if err := s.verifyCompressed(ctx, entry); err != nil {
		return nil, err
	}
	return s.frame, nil
}

func (s *sourceFrames) verifyCompressed(ctx context.Context, entry pack.Entry) error {
	if entry.RawLen == 0 {
		return fmt.Errorf("%w: compressed empty blob", pack.ErrCorrupt)
	}
	// The same window policy packed streams apply: the footer's raw length and
	// the frame header must agree, and the window stays within BlobBytes.
	windowLimit := uint64(max(s.limits.BlobBytes, int64(zstd.MinWindowSize)))
	var header zstd.Header
	if err := header.Decode(s.frame); err != nil {
		return fmt.Errorf("%w: decoding zstd header for blob %s: %w", pack.ErrCorrupt, entry.ID, err)
	}
	if header.HasFCS && header.FrameContentSize != entry.RawLen {
		return fmt.Errorf("%w: zstd content size %d differs from raw length %d for blob %s",
			pack.ErrCorrupt, header.FrameContentSize, entry.RawLen, entry.ID)
	}
	window := header.WindowSize
	if header.SingleSegment {
		window = max(header.FrameContentSize, uint64(zstd.MinWindowSize))
	}
	if window > windowLimit {
		return newLimitError(LimitBlobWindowBytes, window, windowLimit)
	}
	if s.decoder == nil {
		decoder, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(windowLimit),
			zstd.WithDecoderMaxWindow(windowLimit))
		if err != nil {
			return fmt.Errorf("packstore: initialize frame decoder: %w", err)
		}
		s.decoder = decoder
		s.copyBuf = make([]byte, 64<<10)
	}
	// A bytes.Reader keeps the decoder streaming: it decodes whole buffers
	// that expose Bytes into memory.
	if err := s.decoder.Reset(bytes.NewReader(s.frame)); err != nil {
		return fmt.Errorf("%w: opening zstd stream for blob %s: %w", pack.ErrCorrupt, entry.ID, err)
	}
	digest := sha256.New()
	// Reading one byte past RawLen proves the frame decodes to exactly RawLen.
	decoded := io.LimitReader(&contextio.Reader{Context: ctx, Reader: s.decoder}, int64(entry.RawLen)+1)
	n, err := io.CopyBuffer(digest, decoded, s.copyBuf)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w: decoding blob %s: %w", pack.ErrCorrupt, entry.ID, err)
	}
	if uint64(n) != entry.RawLen {
		return fmt.Errorf("%w: blob %s decoded to %d bytes, expected %d", pack.ErrCorrupt, entry.ID, n, entry.RawLen)
	}
	if pack.BlobID(digest.Sum(nil)) != entry.ID {
		return fmt.Errorf("%w: blob %s", pack.ErrBlobMismatch, entry.ID)
	}
	return nil
}

// prepare streams entry through PrepareBlob for frames too large to copy in
// memory. The packed stream verifies CRC, length, and SHA-256 at EOF.
func (s *sourceFrames) prepare(ctx context.Context, indexed IndexEntry, entry pack.Entry, scratchDir string) (*pack.PreparedBlob, error) {
	stream, err := s.reader.OpenBlob(ctx, indexed.Hash)
	if err != nil {
		return nil, mapPackStreamLimit(err)
	}
	prepared, prepareErr := pack.PrepareBlob(ctx, stream, entry.RawLen, pack.DefaultZstdLevel, pack.AppendStreamOptions{
		ExpectedID: &entry.ID, ScratchDir: scratchDir,
	})
	streamErr := stream.Close()
	if err := errors.Join(prepareErr, streamErr); err != nil {
		if prepared != nil {
			_ = prepared.Close()
		}
		return nil, err
	}
	return prepared, nil
}

func (s *sourceFrames) closeReader() {
	if s.reader != nil {
		// Reads hold no state that a close failure could lose.
		_ = s.reader.Close()
		s.reader, s.packID = nil, ""
	}
}

func (s *sourceFrames) Close() {
	s.closeReader()
	if s.decoder != nil {
		s.decoder.Close()
		s.decoder = nil
	}
}
