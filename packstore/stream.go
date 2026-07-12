package packstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sync"

	"go.kenn.io/kit/pack"
)

// VerifiedReadCloser is trusted only after terminal io.EOF, Verify succeeds,
// or Verified returns true. Close does not drain an incomplete stream.
type VerifiedReadCloser interface {
	io.ReadCloser
	Verify() error
	Verified() bool
}

// OpenStream returns catalog-authorized loose or packed content without a
// whole-object allocation. Resolution is retried once if migration removes the
// initially selected physical source.
func (s *Store) OpenStream(ctx context.Context, contentHash Hash) (VerifiedReadCloser, int64, error) {
	if ctx == nil {
		return nil, 0, fmt.Errorf("packstore: nil context")
	}
	if err := contentHash.Validate(); err != nil {
		return nil, 0, err
	}
	return resolveBlob(ctx, s, contentHash,
		func(hash Hash) (VerifiedReadCloser, int64, error) { return s.openLooseStream(ctx, hash) },
		func(hash Hash, entry *IndexEntry) (VerifiedReadCloser, int64, error) {
			return s.openPackedStream(ctx, hash, entry)
		})
}

// CopyVerified copies catalog-authorized content to caller-owned private
// staging. It neither closes nor syncs dst and succeeds only after verified
// EOF; publication remains a separate caller responsibility.
func (s *Store) CopyVerified(ctx context.Context, contentHash Hash, dst io.Writer) (written int64, resultErr error) {
	if dst == nil {
		return 0, fmt.Errorf("packstore: nil verified-copy destination")
	}
	stream, _, err := s.OpenStream(ctx, contentHash)
	if err != nil {
		return 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, stream.Close()) }()
	written, resultErr = io.CopyBuffer(dst, stream, make([]byte, 64<<10))
	return written, resultErr
}

func (s *Store) openPackedStream(
	ctx context.Context, contentHash Hash, indexed *IndexEntry,
) (VerifiedReadCloser, int64, error) {
	slot, footer, release, err := s.acquirePackedEntry(contentHash, indexed, true)
	if err != nil {
		return nil, 0, err
	}
	if err := s.validatePackPolicy(slot); err != nil {
		return nil, 0, errors.Join(err, release())
	}
	limit := uint64(s.limits.BlobBytes) //nolint:gosec // validated non-negative
	if footer.RawLen > limit {
		return nil, 0, errors.Join(newLimitError(LimitBlobRawBytes, footer.RawLen, limit), release())
	}
	if footer.StoredLen > limit {
		return nil, 0, errors.Join(newLimitError(LimitBlobStoredBytes, footer.StoredLen, limit), release())
	}
	stream, err := slot.reader.OpenBlob(ctx, footer)
	if err != nil {
		return nil, 0, errors.Join(mapPackStreamLimit(err), release())
	}
	return &packedVerifiedStream{reader: stream, release: release}, int64(footer.RawLen), nil //nolint:gosec // MaxRawLen fits int64
}

type packedVerifiedStream struct {
	reader     *pack.BlobReader
	release    func() error
	once       sync.Once
	close      error
	releaseErr error
}

func (s *packedVerifiedStream) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	if err == nil {
		return n, nil
	}
	s.finish()
	if errors.Is(err, io.EOF) && s.close == nil && s.releaseErr == nil {
		return n, io.EOF
	}
	return n, errors.Join(err, s.releaseErr)
}

func (s *packedVerifiedStream) Verify() error {
	err := s.reader.Verify()
	s.finish()
	return errors.Join(err, s.releaseErr)
}

func (s *packedVerifiedStream) Verified() bool { return s.reader.Verified() }

func (s *packedVerifiedStream) Close() error {
	s.finish()
	return errors.Join(s.close, s.releaseErr)
}

func (s *packedVerifiedStream) finish() {
	s.once.Do(func() {
		s.close = s.reader.Close()
		s.releaseErr = s.release()
	})
}

func (s *Store) openLooseStream(ctx context.Context, contentHash Hash) (VerifiedReadCloser, int64, error) {
	f, size, err := s.openLoose(contentHash)
	if err != nil {
		return nil, 0, err
	}
	if size < 0 {
		return nil, 0, errors.Join(fmt.Errorf("packstore: negative loose size %d", size), f.Close())
	}
	if size > s.limits.BlobBytes {
		return nil, 0, errors.Join(
			newLimitError(LimitBlobRawBytes, uint64(size), uint64(s.limits.BlobBytes)), //nolint:gosec
			f.Close())
	}
	id, err := pack.ParseBlobID(contentHash.String())
	if err != nil {
		return nil, 0, errors.Join(err, f.Close())
	}
	return &looseVerifiedStream{
		ctx: ctx, f: f, expected: id, size: uint64(size), digest: sha256.New(),
	}, size, nil
}

type looseVerifiedStream struct {
	ctx      context.Context
	f        *os.File
	expected pack.BlobID
	size     uint64
	digest   hash.Hash
	read     uint64
	terminal error
	closeErr error
	verified bool
	closed   bool
}

func (s *looseVerifiedStream) Read(p []byte) (int, error) {
	if s.closed {
		if s.terminal != nil {
			return 0, s.terminal
		}
		if s.verified {
			return 0, io.EOF
		}
		return 0, os.ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := s.ctx.Err(); err != nil {
		return 0, s.fail(err)
	}
	remaining := s.size - s.read
	if remaining == 0 {
		return 0, s.finish()
	}
	want := min(uint64(len(p)), remaining)
	n, readErr := s.f.Read(p[:want])
	if n > 0 {
		_, _ = s.digest.Write(p[:n])
		s.read += uint64(n)
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return n, s.fail(readErr)
	}
	if errors.Is(readErr, io.EOF) && s.read != s.size {
		return n, s.fail(fmt.Errorf("%w: loose content ended at %d bytes, expected %d",
			ErrContentMismatch, s.read, s.size))
	}
	if s.read == s.size {
		return n, s.finish()
	}
	if n == 0 {
		return 0, s.fail(io.ErrNoProgress)
	}
	return n, nil
}

func (s *looseVerifiedStream) finish() error {
	if err := s.ctx.Err(); err != nil {
		return s.fail(err)
	}
	var probe [1]byte
	n, err := s.f.Read(probe[:])
	if n != 0 || err == nil {
		return s.fail(newLimitError(LimitBlobStatBytes, s.size+uint64(n), s.size))
	}
	if !errors.Is(err, io.EOF) {
		return s.fail(err)
	}
	var got pack.BlobID
	copy(got[:], s.digest.Sum(nil))
	if got != s.expected {
		return s.fail(fmt.Errorf("%w: loose hash differs from %s", ErrContentMismatch, s.expected))
	}
	s.closeErr = s.f.Close()
	s.closed = true
	if s.closeErr != nil {
		s.terminal = s.closeErr
		return s.terminal
	}
	s.verified = true
	return io.EOF
}

func (s *looseVerifiedStream) fail(err error) error {
	if s.terminal == nil {
		s.closeErr = s.f.Close()
		s.closed = true
		s.terminal = errors.Join(err, s.closeErr)
	}
	return s.terminal
}

func (s *looseVerifiedStream) Verify() error {
	if s.verified {
		return nil
	}
	if s.terminal != nil {
		return s.terminal
	}
	if s.closed {
		return os.ErrClosed
	}
	buf := make([]byte, 64<<10)
	for {
		_, err := s.Read(buf)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *looseVerifiedStream) Verified() bool { return s.verified }

func (s *looseVerifiedStream) Close() error {
	if !s.closed {
		s.closeErr = s.f.Close()
		s.closed = true
	}
	if s.verified {
		return s.closeErr
	}
	return errors.Join(s.terminal, s.closeErr, pack.ErrVerificationIncomplete)
}
