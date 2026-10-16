package gcs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v7"
	"go.kenn.io/kit/internal/contextio"
	"go.kenn.io/kit/objectstore"
)

// Publish validates the whole rewindable source before creating a resumable
// session. Callers own the source and keep it stable until this call returns.
// A receipt proves the immutable key's bytes, regardless of who created them.
func (s *Store) Publish(ctx context.Context, key string, src io.ReadSeeker, size int64, digest [32]byte) (objectstore.Receipt, error) {
	full, err := s.key(key)
	if err != nil {
		return objectstore.Receipt{}, err
	}
	if src == nil || size < 0 {
		return objectstore.Receipt{}, fmt.Errorf("gcs: invalid source or size: %w", objectstore.ErrChecksumMismatch)
	}
	crc, err := validateSource(ctx, src, size, digest)
	if err != nil {
		return objectstore.Receipt{}, err
	}
	headers := http.Header{"x-goog-resumable": {"start"}, "x-goog-if-generation-match": {"0"}, "Content-Type": {"application/octet-stream"}}
	response, _, err := s.request(ctx, http.MethodPost, s.objectURL(full), nil, 0, headers)
	// Initiation cannot publish bytes; its transport failures stay ordinary.
	if err != nil {
		return objectstore.Receipt{}, err
	}
	if err := responseError(response); err != nil {
		if errors.Is(err, objectstore.ErrRevisionChanged) {
			return s.verify(ctx, key, size, digest, crc, true, "")
		}
		return objectstore.Receipt{}, err
	}
	location := response.Header.Get("Location")
	closeErr := response.Body.Close()
	if closeErr != nil {
		return objectstore.Receipt{}, closeErr
	}
	session, err := url.Parse(location)
	if err != nil || !session.IsAbs() || session.Host != s.endpoint.Host || session.Scheme != s.endpoint.Scheme || session.User != nil || session.Fragment != "" {
		return objectstore.Receipt{}, errors.New("gcs: invalid resumable session location")
	}
	generation, reconcile, err := s.upload(ctx, session, src, size)
	if err != nil && !reconcile {
		return objectstore.Receipt{}, err
	}
	receipt, verifyErr := s.verify(ctx, key, size, digest, crc, reconcile, generation)
	if verifyErr != nil {
		return objectstore.Receipt{}, errors.Join(verifyErr, err)
	}
	return receipt, nil
}

func validateSource(ctx context.Context, src io.ReadSeeker, size int64, digest [32]byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("gcs: rewind source: %w", err)
	}
	sha := sha256.New()
	crc := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	reader := &contextio.Reader{Context: ctx, Reader: src}
	n, err := io.CopyBuffer(io.MultiWriter(sha, crc), io.LimitReader(reader, size), make([]byte, 64<<10))
	if err != nil {
		return "", err
	}
	var tail [1]byte
	more, tailErr := io.ReadFull(reader, tail[:])
	if tailErr != nil && !errors.Is(tailErr, io.EOF) {
		return "", tailErr
	}
	if n != size || more != 0 || string(sha.Sum(nil)) != string(digest[:]) {
		return "", objectstore.ErrChecksumMismatch
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("gcs: rewind validated source: %w", err)
	}
	return base64.StdEncoding.EncodeToString(crc.Sum(nil)), nil
}

func (s *Store) upload(ctx context.Context, session *url.URL, src io.ReadSeeker, size int64) (generation string, reconcile bool, resultErr error) {
	var offset int64
	recovery, uncertain, complete := false, false, false
	defer func() {
		if complete || uncertain {
			return
		}
		// Only this operation's session is eligible for abort. No background budget.
		response, _, err := s.request(ctx, http.MethodDelete, session, nil, 0, nil)
		if err == nil {
			if response.StatusCode == http.StatusNoContent {
				err = response.Body.Close()
			} else {
				err = responseError(response)
			}
		}
		resultErr = errors.Join(resultErr, err)
	}()
	retryBackoff := backoff.NewExponentialBackOff()
	retryBackoff.InitialInterval = 10 * time.Millisecond
	retryBackoff.MaxInterval = 100 * time.Millisecond
	_, err := backoff.Retry(ctx, func() (bool, error) {
		for {
			if err := ctx.Err(); err != nil {
				return false, backoff.Permanent(err)
			}
			var body io.Reader
			length := min(int64(8<<20), size-offset)
			final := offset+length == size
			headers := http.Header{}
			if recovery {
				headers.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
				length = 0
			} else {
				if _, err := src.Seek(offset, io.SeekStart); err != nil {
					return false, backoff.Permanent(err)
				}
				body = io.LimitReader(&contextio.Reader{Context: ctx, Reader: src}, length)
				if size > 0 {
					headers.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, size))
				}
			}
			response, sent, requestErr := s.request(ctx, http.MethodPut, session, body, length, headers)
			if requestErr != nil {
				if !recovery && final && sent {
					uncertain = true
				}
				recovery = true
				return false, requestErr
			}
			status := response.StatusCode
			if status == http.StatusOK || status == http.StatusCreated {
				complete = true
				generation = response.Header.Get("x-goog-generation")
				recovered := recovery || uncertain || generation == ""
				if !recovery && !final {
					uncertain = true
					return false, backoff.Permanent(errors.Join(errors.New("gcs: premature resumable completion"), response.Body.Close()))
				}
				if err := response.Body.Close(); err != nil {
					uncertain = true
					return false, backoff.Permanent(err)
				}
				reconcile = recovered
				return true, nil
			}
			if status == http.StatusPermanentRedirect {
				next, progressErr := resumeOffset(response.Header.Get("Range"), size)
				closeErr := response.Body.Close()
				if progressErr != nil || closeErr != nil {
					return false, backoff.Permanent(errors.Join(progressErr, closeErr))
				}
				if next < offset || (!recovery && next > offset+length) {
					return false, backoff.Permanent(errors.New("gcs: invalid resumable progress"))
				}
				if !recovery && next == offset {
					return false, backoff.Permanent(errors.New("gcs: resumable upload made no progress"))
				}
				if next == size && size > 0 {
					return false, backoff.Permanent(errors.New("gcs: resumable upload lacks completion status"))
				}
				offset = next
				recovery = false
				continue
			}
			responseErr := responseError(response)
			if status == http.StatusPreconditionFailed {
				reconcile = true
				return false, backoff.Permanent(responseErr)
			}
			if status >= 500 {
				if !recovery && final {
					uncertain = true
				}
				recovery = true
				return false, responseErr
			}
			return false, backoff.Permanent(responseErr)
		}
	}, backoff.WithBackOff(retryBackoff), backoff.WithMaxTries(3), backoff.WithMaxElapsedTime(0))
	if err != nil {
		err = backoff.AsRetryError(err).LastErr
	}
	return generation, reconcile || uncertain, err
}

func resumeOffset(value string, size int64) (int64, error) {
	if value == "" {
		return 0, nil
	}
	last, ok := strings.CutPrefix(value, "bytes=0-")
	if !ok {
		return 0, errors.New("gcs: invalid resumable range")
	}
	end, err := strconv.ParseInt(last, 10, 64)
	if err != nil || end < 0 || end >= size {
		return 0, errors.New("gcs: invalid resumable range")
	}
	return end + 1, nil
}

func unknown(err error) error { return errors.Join(objectstore.ErrOutcomeUnknown, err) }
func (s *Store) verify(ctx context.Context, key string, size int64, digest [32]byte, crc string, reconcile bool, generation string) (objectstore.Receipt, error) {
	if err := ctx.Err(); err != nil {
		return objectstore.Receipt{}, unknown(err)
	}
	m, err := s.inspect(ctx, key, generation)
	if err != nil {
		return objectstore.Receipt{}, unknown(err)
	}
	mismatch := objectstore.ErrChecksumMismatch
	if reconcile {
		mismatch = objectstore.ErrExists
	}
	if m.Size != size {
		return objectstore.Receipt{}, mismatch
	}
	method := ""
	if !reconcile {
		for _, checksum := range m.Checksums {
			if checksum.Algorithm == "crc32c" && checksum.Scope == "full" {
				decoded, err := base64.StdEncoding.DecodeString(checksum.Value)
				if err != nil || len(decoded) != 4 {
					continue
				}
				if checksum.Value != crc {
					return objectstore.Receipt{}, mismatch
				}
				method = "provider-crc32c"
			}
		}
	}
	if method == "" {
		if err := s.readback(ctx, m, digest); err != nil {
			if errors.Is(err, objectstore.ErrChecksumMismatch) {
				return objectstore.Receipt{}, mismatch
			}
			return objectstore.Receipt{}, unknown(err)
		}
		method = "readback-sha256"
	}
	return objectstore.Receipt{Object: m, SHA256: digest, Verification: method}, nil
}

func (s *Store) readback(ctx context.Context, m objectstore.Metadata, digest [32]byte) error {
	full, err := s.key(m.Key)
	if err != nil {
		return err
	}
	response, _, err := s.request(ctx, http.MethodGet, s.objectURL(full), nil, 0, http.Header{"x-goog-if-generation-match": {m.Revision}})
	if err != nil {
		return err
	}
	if err := responseError(response); err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || response.ContentLength != m.Size || response.Header.Get("x-goog-generation") != m.Revision {
		return errors.Join(errors.New("gcs: readback length or generation changed"), response.Body.Close())
	}
	sha := sha256.New()
	reader := &contextio.Reader{Context: ctx, Reader: response.Body}
	n, readErr := io.CopyBuffer(sha, io.LimitReader(reader, m.Size), make([]byte, 64<<10))
	var tail [1]byte
	more, tailErr := reader.Read(tail[:])
	if errors.Is(tailErr, io.EOF) {
		tailErr = nil
	}
	if err := errors.Join(readErr, tailErr, response.Body.Close()); err != nil {
		return err
	}
	if n != m.Size {
		return io.ErrUnexpectedEOF
	}
	if more != 0 || string(sha.Sum(nil)) != string(digest[:]) {
		return objectstore.ErrChecksumMismatch
	}
	return nil
}
