package selfupdate

import (
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v7"
)

func checksumRetry(ctx context.Context, operation func() ([]byte, error)) ([]byte, error) {
	policy := backoff.NewExponentialBackOff()
	policy.InitialInterval = 500 * time.Millisecond
	policy.MaxInterval = 2 * time.Second
	body, err := backoff.Retry(ctx, operation, backoff.WithBackOff(policy), backoff.WithMaxTries(4), backoff.WithMaxElapsedTime(0))
	if retryErr := backoff.AsRetryError(err); retryErr != nil {
		err = retryErr.LastErr
		if ctx.Err() != nil && errors.Is(retryErr.Cause, context.Cause(ctx)) {
			err = errors.Join(err, ctx.Err(), retryErr.Cause)
		}
	}
	return body, err
}

type checksumTransport struct {
	base   http.RoundTripper
	custom bool
}

func (t checksumTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil && (t.custom || req.URL.Scheme == "http" || req.URL.Scheme == "https") {
		return resp, checksumRequestError{err}
	}
	return resp, err
}

type checksumRequestError struct{ err error }

func (e checksumRequestError) Error() string { return e.err.Error() }
func (e checksumRequestError) Unwrap() error { return e.err }

func (e checksumRequestError) Timeout() bool {
	err, ok := errors.AsType[interface {
		error
		Timeout() bool
	}](e.err)
	return ok && err.Timeout()
}

func checksumTransportError(err error) bool {
	_, transport := errors.AsType[checksumRequestError](err)
	return transport
}

type checksumBody struct{ io.ReadCloser }

func (b checksumBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF && !checksumInvalidContent(err) {
		return n, checksumRequestError{err}
	}
	return n, err
}

func checksumInvalidContent(err error) bool {
	_, corrupt := errors.AsType[flate.CorruptInputError](err)
	return corrupt || errors.Is(err, gzip.ErrHeader) || errors.Is(err, gzip.ErrChecksum)
}

func checksumRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	seconds, err := strconv.ParseInt(header, 10, 64)
	if err == nil {
		return time.Duration(min(max(seconds, 0), 10)) * time.Second
	}
	if errors.Is(err, strconv.ErrRange) && header[0] != '-' {
		return 10 * time.Second
	}
	when, err := http.ParseTime(header)
	if err != nil {
		return 0
	}
	return max(0, min(time.Until(when), 10*time.Second))
}
