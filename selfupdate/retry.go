package selfupdate

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
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
		if errors.Is(retryErr.Cause, context.Canceled) || errors.Is(retryErr.Cause, context.DeadlineExceeded) {
			err = errors.Join(err, retryErr.Cause)
		}
	}
	return body, err
}

func checksumTransportError(err error) bool {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		err = urlErr.Err
	}
	_, network := errors.AsType[net.Error](err)
	return network || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func checksumRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(min(seconds, 10)) * time.Second
	} else if errors.Is(err, strconv.ErrRange) && value[0] != '-' {
		return 10 * time.Second
	}
	date, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	return max(0, min(date.Sub(now), 10*time.Second))
}
